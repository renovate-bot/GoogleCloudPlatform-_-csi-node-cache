// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/GoogleCloudPlatform/csi-node-cache/pkg/common"
	"github.com/GoogleCloudPlatform/csi-node-cache/pkg/util"
)

const (
	nodeCacheNamespace = "node-cache"
)

var (
	kubeconfig = func() *string {
		if home := homedir.HomeDir(); home != "" {
			return flag.String("kubeconfig-path", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
		}
		return flag.String("kubeconfig-path", "", "absolute path to the kubeconfig file")
	}()

	K8sClient       *kubernetes.Clientset
	NodeCacheLabels map[string]bool

	testNamespace string
)

func repoBase() string {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		klog.Fatalf("Could not retrieve caller information")
	}
	// Assume this is called from <repo-base>/e2e.
	e2eDir := filepath.Dir(filename)
	base := filepath.Dir(e2eDir)
	klog.Infof("Using %s as repo base", base)
	return base
}

func collectNodeCacheLabels(ctx context.Context) error {
	nodes, err := K8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("can't list nodes for collecting labels: %w", err)
	}
	NodeCacheLabels = make(map[string]bool)
	for _, node := range nodes.Items {
		labels := node.GetLabels()
		kind, found := labels[common.VolumeTypeLabel]
		if found {
			klog.Infof("node/%s has %s=%s", node.GetName(), common.VolumeTypeLabel, kind)
			NodeCacheLabels[kind] = true
		}
	}
	return nil
}

func skipUnlessLabeled(t *testing.T, label string) {
	t.Helper()
	if _, found := NodeCacheLabels[label]; !found {
		t.Skip(fmt.Sprintf("Skipping %s as no nodes with %s=%s", t.Name(), common.VolumeTypeLabel, label))
	}
}

func restartDriver(ctx context.Context, t *testing.T) {
	t.Helper()
	pods, err := K8sClient.CoreV1().Pods(nodeCacheNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("couldn't list node cache pods to restart driver: %v", err)
	}
	for _, p := range pods.Items {
		for _, owner := range p.GetOwnerReferences() {
			if owner.Kind == "DaemonSet" && owner.Name == "driver" {
				if err := K8sClient.CoreV1().Pods(nodeCacheNamespace).Delete(ctx, p.GetName(), metav1.DeleteOptions{}); err != nil {
					t.Fatalf("could not delete %s to restart driver: %v", p.GetName(), err)
				}
				t.Logf("restarted driver pod %s", p.GetName())
				break
			}
		}
	}
}

func runOnPod(ctx context.Context, t *testing.T, pod *corev1.Pod, cmd string, args ...string) (string, error) {
	t.Helper()
	output, err := util.RunCommand("kubectl", slices.Concat([]string{
		"exec",
		fmt.Sprintf("--namespace=%s", testNamespace),
		pod.GetName(),
		"--",
		cmd,
	}, args)...)
	return string(output), err
}

func runOnNode(ctx context.Context, t *testing.T, node, cmd string, args ...string) (string, error) {
	t.Helper()
	zone := nodeZone(ctx, node)
	cmd = fmt.Sprintf("--command=sudo %s %s", cmd, strings.Join(args, " "))
	var cmdOutput string
	// gcloud compute ssh can be flaky if a proxy is used, so we retry a couple of times.
	err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		output, err := util.RunCommand("gcloud", "compute", "ssh", "--zone", zone, node, cmd)
		cmdOutput = string(output)
		t.Logf("on %s ran %s %s: %s", node, cmd, strings.Join(args, " "), string(output))
		if err != nil && strings.HasPrefix(cmdOutput, "RPC AclTests failed") {
			t.Logf("proxy error, retrying")
			return false, nil
		}
		return true, err
	})
	return cmdOutput, err
}

func startCachePod(ctx context.Context, t *testing.T, name, cacheType string) *corev1.Pod {
	t.Helper()
	return startCmdPodExtended(ctx, t, name, "/cache", map[string]string{
		common.VolumeTypeLabel: cacheType,
	})
}

func startCachePodOnNode(ctx context.Context, t *testing.T, name, node string) *corev1.Pod {
	t.Helper()
	return startCmdPodExtended(ctx, t, name, "/cache", map[string]string{
		"kubernetes.io/hostname": node,
	})
}

func buildCmdPod(name, cacheMount string, nodeSelector map[string]string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: ptr.To(int64(1)),
			Containers: []corev1.Container{
				{
					Name:    "main",
					Image:   "debian",
					Command: []string{"sleep", "900"},
				},
			},
		},
	}
	if cacheMount != "" {
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
			{
				Name:      "cache",
				MountPath: cacheMount,
			},
		}
		pod.Spec.Volumes = []corev1.Volume{
			{
				Name: "cache",
				VolumeSource: corev1.VolumeSource{
					CSI: &corev1.CSIVolumeSource{
						Driver: "node-cache.csi.storage.gke.io",
					},
				},
			},
		}
	}
	if nodeSelector != nil {
		pod.Spec.NodeSelector = nodeSelector
	}
	return pod
}

func startCmdPodExtended(ctx context.Context, t *testing.T, name, cacheMount string, nodeSelector map[string]string) *corev1.Pod {
	t.Helper()
	t.Logf("%v: starting pod %s", time.Now(), name)

	pod := buildCmdPod(name, cacheMount, nodeSelector)

	pod, err := K8sClient.CoreV1().Pods(pod.GetNamespace()).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Creating pod/%s: %v", name, err)
	}
	var runningPod *corev1.Pod
	if err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		var err error
		runningPod, err = K8sClient.CoreV1().Pods(pod.GetNamespace()).Get(ctx, pod.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Logf("waiting for pod/%s: %v", name, err)
			return false, nil // retry
		}
		if runningPod.Status.Phase == corev1.PodFailed || runningPod.Status.Phase == corev1.PodSucceeded {
			return false, fmt.Errorf("Unexpected exit for pod/%s: %v", name, runningPod.Status.Phase)
		}
		if runningPod.Status.Phase != corev1.PodRunning {
			return false, nil // retry
		}
		return true, nil
	}); err != nil {
		t.Fatalf("Waiting for pod/%s runnable: %v", name, err)
	}
	if runningPod.Spec.NodeName == "" {
		t.Fatalf("pod/%s running, but not assigned a node?", name)
	}
	t.Logf("%v: started %s", time.Now(), name)
	return runningPod
}

func nodeZone(ctx context.Context, nodeName string) string {
	node, err := K8sClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		klog.Fatal(err.Error())
	}
	zone := node.GetLabels()["topology.kubernetes.io/zone"]
	if zone == "" {
		klog.Fatalf("missing zone for %s", nodeName)
	}
	return zone
}

func currentProject() string {
	output, err := util.RunCommand("gcloud", "config", "list")
	if err != nil {
		klog.Fatal(err.Error())
	}
	for _, line := range strings.Split(string(output), "\n") {
		line := strings.TrimSpace(line)
		if strings.HasPrefix(line, "project =") {
			items := strings.Split(line, "=")
			if len(items) != 2 {
				klog.Fatalf("bad gcloud config %v", output)
			}
			return strings.TrimSpace(items[1])
		}
	}
	klog.Fatalf("no project found in %v", output)
	return ""
}

func deleteNode(ctx context.Context, t *testing.T, node string) {
	zone := nodeZone(ctx, node)
	t.Logf("%v: deleting node %s", time.Now(), node)
	var errors []string
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := util.RunCommand("gcloud", "compute", "instances", "delete", "--zone", zone, node, "--quiet")
		if err == nil {
			return true, nil
		} else if strings.Contains(err.Error(), "was not found") {
			return true, nil
		} else if strings.Contains(err.Error(), "Please try again in 30 seconds") {
			klog.Errorf("gcloud delete of %s needs a break, pausing", node)
			time.Sleep(30 * time.Second)
			return false, nil
		} else {
			klog.Errorf("gcloud delete node %s failed, retrying: %v", node, err)
			return false, nil
		}
	})
	assert.NilError(t, err, errors)
	t.Logf("%v: %s deleted", time.Now(), node)
}

func deletePod(ctx context.Context, t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if err := K8sClient.CoreV1().Pods(pod.GetNamespace()).Delete(ctx, pod.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting pod/%s: %v", pod.GetName(), err)
	}
	var lastErr error
	if err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		if _, err := K8sClient.CoreV1().Pods(pod.GetNamespace()).Get(ctx, pod.GetName(), metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return true, nil
		} else if err != nil {
			lastErr = err
		}
		return false, nil // retry
	}); err != nil {
		t.Fatalf("delete pod: %v (%v)", err, lastErr)
	}
}

func testNamespaceSetup(ctx context.Context, t *testing.T) func() {
	testNamespace = fmt.Sprintf("ns-%d", rand.Uint32())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
	if _, err := K8sClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		klog.Fatalf("Could not create test namespace %s: %v", testNamespace, err)
	}
	return func() {
		t.Logf("%v: test completed, tearing down", time.Now())
		if err := K8sClient.CoreV1().Namespaces().Delete(ctx, ns.GetName(), metav1.DeleteOptions{}); err != nil {
			klog.Errorf("Error deleting test namespace %s: %v", testNamespace, err)
		}
		t.Logf("%v: tore down namespace/%s", time.Now(), testNamespace)
	}
}

func cleanUpPds(ctx context.Context) error {
	// Since we're racing with the controller tearing down, a finalizer that we remove might get replaced, so we
	// check until there's no PVCs left.
	klog.Info("removing pd finalizers")
	pvs := map[string]string{} // pv -> node name (which is also the pvc name).
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		pvcs, err := K8sClient.CoreV1().PersistentVolumeClaims(nodeCacheNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		foundPVC := false
		for _, pvc := range pvcs.Items {
			foundPVC = true

			pvs[pvc.Spec.VolumeName] = pvc.GetName()

			found := false
			newFinalizers := []string{}
			for _, finalizer := range pvc.Finalizers {
				if finalizer == "node-cache.gke.io/in-use" {
					found = true
				} else {
					newFinalizers = append(newFinalizers, finalizer)
				}
			}
			if found {
				klog.Infof("removing finalizer for %s", pvc.GetName())
				pvc.Finalizers = newFinalizers
				_, err := K8sClient.CoreV1().PersistentVolumeClaims(nodeCacheNamespace).Update(ctx, &pvc, metav1.UpdateOptions{})
				if err != nil {
					klog.Warningf("couldn't update finalizer for %s: %v", pvc.GetName(), err)
				}
			}
		}
		return !foundPVC, nil
	})
	if err != nil {
		return fmt.Errorf("could not remove finalizers: %w", err)
	}

	klog.Infof("Detaching any PVs")
	// The PVs are probably still attached to nodes, and must be manually detached.
	computeSvc, err := compute.NewService(ctx)
	if err != nil {
		return err
	}
	project := currentProject()
	err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		deleted := []string{}
		for pv, node := range pvs {
			zone := nodeZone(ctx, node)
			// Since the PVs are all dynamically created, we know the PV name is the disk name. If the provisioning changes,
			// this assumption could be broken.
			op, err := computeSvc.Instances.DetachDisk(project, zone, node, pv).Context(ctx).Do()
			if err != nil {
				klog.Warningf("error detaching %s from %s, retrying: %v", pv, node, err)
				continue
			}
			err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
				pollOp, err := computeSvc.ZoneOperations.Get(project, zone, op.Name).Context(ctx).Do()
				if err != nil {
					return false, err
				}
				if pollOp == nil || pollOp.Status != "DONE" {
					return false, nil // retry
				}
				if pollOp.Error != nil {
					errs := []string{}
					for _, e := range pollOp.Error.Errors {
						errs = append(errs, fmt.Sprintf("%v", e))
					}
					return false, fmt.Errorf("error waiting to detach from %s: %v", node, errs)
				}
				return true, nil
			})
			if err == nil {
				deleted = append(deleted, pv)
			}
		}
		for _, pv := range deleted {
			delete(pvs, pv)
		}
		return len(pvs) == 0, nil
	})
	if err != nil {
		return fmt.Errorf("could not detach pvs: %w", err)
	}
	klog.Info("pds cleaned up")
	return nil
}

func mustDeployDriver() {
	base := repoBase()
	out, err := util.RunCommand("kubectl", "apply", "-k", filepath.Join(base, "deploy"))
	if err != nil {
		klog.Fatalf("Could not deploy driver: %v", err)
	}
	klog.Info(string(out))
}

func mustTearDownDriver(ctx context.Context) {
	klog.Infof("tearing down test infrastructure")
	base := repoBase()
	if _, err := util.RunCommand("kubectl", "delete", "--wait=false", "-k", filepath.Join(base, "deploy")); err != nil {
		klog.Errorf("Error tearing down driver: %v", err)
	}
	cleanUpPds(ctx)
	if err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := K8sClient.CoreV1().Namespaces().Get(ctx, nodeCacheNamespace, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}); err != nil {
		klog.Errorf("Error waiting for driver namespace to disappear: %v", err)
		os.Exit(1)
	}
	klog.Infof("tore down driver")
}

func TestMain(m *testing.M) {
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		klog.Fatalf("Could not build kubeconfig: %v", err)
	}
	K8sClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Could not get k8s client: %v", err)
	}
	ctx := context.Background()

	if err := collectNodeCacheLabels(ctx); err != nil {
		klog.Fatal(err.Error())
	}

	mustDeployDriver()

	retval := m.Run()

	mustTearDownDriver(ctx)

	os.Exit(retval)
}
