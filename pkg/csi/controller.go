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

package csi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	finalizerLabel = "node-cache.gke.io/in-use"
	zoneLabel      = "topology.gke.io/zone"
)

type volumeHandle struct {
	project string
	zone    string
	name    string
}

type reconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	k8sClient           *kubernetes.Clientset
	namespace           string
	volumeTypeConfigMap string
	pdStorageClass      string
	attacher            Attacher
}

type pvcReconciler struct {
	*reconciler
}

type Attacher interface {
	diskIsAttached(ctx context.Context, volume, nodeName string) (bool, error)
	attachDisk(ctx context.Context, volume, nodeName string) error
}

type attacher struct {
	k8sClient  client.Client
	computeSvc *compute.Service
}

var _ Attacher = &attacher{}

func NewAttacher(ctx context.Context, cfg *rest.Config) (Attacher, error) {
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, err
	}
	svc, err := compute.NewService(ctx)
	if err != nil {
		return nil, err
	}
	return &attacher{k8sClient: k8sClient, computeSvc: svc}, nil
}

func ControllerInit() {
	// This should get all core objects.
	utilruntime.Must(scheme.AddToScheme(scheme.Scheme))
}

func NewManager(cfg *rest.Config, namespace, volumeTypeConfigMap string, attach Attacher, pdStorageClass string) (ctrl.Manager, error) {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				namespace: {},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("unable to create manager: %w", err)
	}
	k8sClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to create k8s client: %w", err)
	}
	rec := &reconciler{
		Client:              mgr.GetClient(),
		k8sClient:           k8sClient,
		Scheme:              mgr.GetScheme(),
		namespace:           namespace,
		volumeTypeConfigMap: volumeTypeConfigMap,
		pdStorageClass:      pdStorageClass,
		attacher:            attach,
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("node").
		Watches(&corev1.Node{}, &handler.EnqueueRequestForObject{}).
		Complete(rec); err != nil {
		return nil, err
	}
	if rec.attacher != nil {
		if err := ctrl.NewControllerManagedBy(mgr).
			Named("pvc").
			Watches(&corev1.PersistentVolumeClaim{}, &handler.EnqueueRequestForObject{}).
			Complete(&pvcReconciler{rec}); err != nil {
			return nil, err
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("Unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("Unable to set up ready check: %w", err)
	}
	return mgr, nil
}

func (r *reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		log.Error(err, "get node for reconcile", "node", req.NamespacedName.Name)
		r.deleteOrphanedPDs(ctx)
		return ctrl.Result{}, nil
	}

	if node.DeletionTimestamp != nil {
		r.deleteOrphanedPDs(ctx)
		// TODO: clean up old mappings?
		return ctrl.Result{}, nil
	}

	mustCreateMapping := false
	var mapping map[string]volumeTypeInfo
	var configMap corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: r.volumeTypeConfigMap}, &configMap)
	if apierrors.IsNotFound(err) {
		mustCreateMapping = true
		configMap.SetNamespace(r.namespace)
		configMap.SetName(r.volumeTypeConfigMap)
		mapping = map[string]volumeTypeInfo{}
	} else if err == nil {
		if mapping, err = getVolumeTypeMapping(configMap.Data); err != nil {
			log.Error(err, "bad mapping (ignored, mapping recreated)")
			mapping = map[string]volumeTypeInfo{}
		}
	} else {
		log.Error(err, "get mapping", "mapping", fmt.Sprintf("%s/%s", r.namespace, r.volumeTypeConfigMap))
		return ctrl.Result{}, nil
	}

	if configMap.Data == nil {
		configMap.Data = map[string]string{}
	}

	info, err := getVolumeTypeFromNode(&node)
	if err != nil && strings.Contains(err.Error(), "label not found on node") {
		log.Info("skipping non-cache node", "node", node.GetName())
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}

	if info.VolumeType == pdVolumeType {
		if r.pdStorageClass == "" {
			return ctrl.Result{}, fmt.Errorf("No PD storage class has been defined, PD volumes can't be used")
		}
		if err := r.updatePdVolumeType(ctx, node.GetName(), &info); err != nil {
			return ctrl.Result{}, err
		}
	}

	mapping[node.GetName()] = info
	if err := writeVolumeTypeMapping(configMap.Data, mapping); err != nil {
		log.Error(err, "write mapping", "node", node.GetName())
		return ctrl.Result{}, err

	}
	if mustCreateMapping {
		if err := r.Create(ctx, &configMap); err != nil {
			log.Error(err, "create configmap")
			return ctrl.Result{}, err // requeue
		}
	} else {
		if err := r.Update(ctx, &configMap); err != nil {
			log.Error(err, "update configmap")
			return ctrl.Result{}, err // requeue
		}
	}
	log.Info("update", "node", node.GetName(), "info", info)

	return ctrl.Result{}, nil
}

func (r *reconciler) updatePdVolumeType(ctx context.Context, node string, info *volumeTypeInfo) error {
	if info.VolumeType != pdVolumeType {
		return nil
	}

	if info.Size.IsZero() {
		return fmt.Errorf("no size given for PD cache on node %s", node)
	}

	var pvc corev1.PersistentVolumeClaim
	needCreate := false
	err := r.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: node}, &pvc)
	if apierrors.IsNotFound(err) {
		storageClass, err := r.getStorageClassForNode(ctx, node)
		if err != nil {
			return fmt.Errorf("Cannot get storage class for %s: %w", node, err)
		}
		needCreate = true
		pvc.SetName(node)
		pvc.SetNamespace(r.namespace)
		pvc.Spec.StorageClassName = ptr.To(storageClass)
		pvc.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
		pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		pvc.Spec.Resources.Requests = map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceStorage: info.Size,
		}
	} else if err == nil && pvc.Status.Phase == corev1.ClaimBound {
		info.Disk = pvc.Spec.VolumeName
	}

	return r.updatePVCForLifecycle(ctx, &pvc, needCreate)
}

func (r *reconciler) getStorageClassForNode(ctx context.Context, nodeName string) (string, error) {
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return "", err
	}
	var zone string
	for label, val := range node.GetLabels() {
		if label == zoneLabel {
			zone = val
			break
		}
	}
	if zone == "" {
		log.FromContext(ctx).Error(nil, "No node zone label, will use zone-independent storage class. Probably you're running on a non-GKE cluster", "node", nodeName)
		return r.pdStorageClass, nil
	}
	scName := fmt.Sprintf("%s-%s", r.pdStorageClass, zone)
	var sc storagev1.StorageClass
	err := r.Get(ctx, types.NamespacedName{Name: scName}, &sc)
	if apierrors.IsNotFound(err) {
		if err := r.Get(ctx, types.NamespacedName{Name: r.pdStorageClass}, &sc); err != nil {
			return "", fmt.Errorf("Cannot get template storage class %s: %w", r.pdStorageClass, err)
		}
		// Reset most of object meta to avoid creation errors.
		sc.ObjectMeta = metav1.ObjectMeta{
			Name:        scName,
			Labels:      sc.GetLabels(),
			Annotations: sc.GetAnnotations(),
		}

		sc.AllowedTopologies = []corev1.TopologySelectorTerm{
			{
				MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{
					{
						Key: zoneLabel,
						Values: []string{
							zone,
						},
					},
				},
			},
		}
		if err := r.Create(ctx, &sc); err != nil {
			return "", fmt.Errorf("Could not create zone-specific storage class %s: %w", scName, err)
		}
	} else if err != nil {
		return "", fmt.Errorf("Could not look up zone-specific storage class %s: %w", scName, err)
	}
	return scName, nil
}

func (r *reconciler) updatePVCForLifecycle(ctx context.Context, pvc *corev1.PersistentVolumeClaim, needCreate bool) error {
	found := false
	for _, finalizer := range pvc.Finalizers {
		if finalizer == finalizerLabel {
			found = true
			break
		}
	}

	changed := false

	if !found {
		changed = true
		pvc.Finalizers = append(pvc.Finalizers, finalizerLabel)
	}
	if needCreate {
		if err := r.Create(ctx, pvc); err != nil {
			return err
		}
	} else if changed {
		if err := r.Update(ctx, pvc); err != nil {
			return err
		}
	}
	return nil
}

func (r *pvcReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	pvcName := req.NamespacedName.Name

	var configMap corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: r.volumeTypeConfigMap}, &configMap)
	if err != nil {
		log.Info("PVC reconcile before mapping available", "pvc", pvcName, "error", err)
		return ctrl.Result{Requeue: true}, nil
	}
	mapping, err := getVolumeTypeMapping(configMap.Data)
	if err != nil {
		return ctrl.Result{}, err
	}

	info, found := mapping[pvcName]
	if !found {
		return ctrl.Result{}, fmt.Errorf("Unknown node or pvc %s", pvcName)
	}

	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, req.NamespacedName, &pvc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling %s: %w", req.NamespacedName, err)
	}

	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: pvcName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			node.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		} else {
			return ctrl.Result{}, fmt.Errorf("can't get node %s for pvc: %w", pvc.GetName(), err)
		}
	}
	if node.DeletionTimestamp != nil {
		// The node doesn't exist, the PVC should be deleted.
		return ctrl.Result{}, r.deletePVC(ctx, &pvc)
	}

	mustRequeue := false

	// Update the mapping with the PV name, if known.
	if pvc.Status.Phase == corev1.ClaimBound && info.Disk != pvc.Spec.VolumeName {
		if info.Disk != "" && info.Disk != pvc.Spec.VolumeName {
			log.Error(nil, "pv mapping mismatch, will update", "old-disk", info.Disk, "curr-diisk", pvc.Spec.VolumeName)
		}
		info.Disk = pvc.Spec.VolumeName
		mapping[pvcName] = info
		if err := writeVolumeTypeMapping(configMap.Data, mapping); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Update(ctx, &configMap); err != nil {
			log.Error(err, "mapping update, will requeue")
			mustRequeue = true
		}
	}

	// If the PVC is bound but not attached, attach it.
	if pvc.Status.Phase == corev1.ClaimBound {
		var pv corev1.PersistentVolume
		if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
			return ctrl.Result{}, fmt.Errorf("Can't get volume for pvc %s: %w", pvc.GetName(), err)
		}
		attached, err := r.attacher.diskIsAttached(ctx, pv.Spec.CSI.VolumeHandle, node.GetName())
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("Could not check attachment for pvc %s, pv %s: %w", pvc.GetName(), pv.GetName(), err)
		}
		if !attached {
			if err := r.attacher.attachDisk(ctx, pv.Spec.CSI.VolumeHandle, node.GetName()); err != nil {
				return ctrl.Result{}, fmt.Errorf("Could not attach pv %s to node %s: %w", pv.GetName(), pvc.GetName(), err)
			}
			log.Info("attach", "pvc", pvc.GetName())
		}
	}

	// Otherwise everything looks good.
	log.Info("reconciled, looks good", "pvc", req.NamespacedName)
	return ctrl.Result{Requeue: mustRequeue}, nil
}

func (r *reconciler) deletePVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	if err := r.Delete(ctx, pvc); err != nil {
		return fmt.Errorf("Delete of pvc/%s failed: %w", pvc.GetName(), err)
	}
	changed := false
	finalizers := []string{}
	for _, f := range pvc.Finalizers {
		if f == finalizerLabel {
			changed = true
		} else {
			finalizers = append(finalizers, f)
		}
	}
	if changed {
		pvc.Finalizers = finalizers
		return r.Update(ctx, pvc)
	}
	return nil
}

func (r *reconciler) deleteOrphanedPDs(ctx context.Context) error {
	var pvcs corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcs); err != nil {
		return err
	}
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return err
	}
	knownNodes := make(map[string]bool, len(nodes.Items))
	for _, n := range nodes.Items {
		if n.DeletionTimestamp == nil {
			knownNodes[n.GetName()] = true
		}
	}
	for _, pvc := range pvcs.Items {
		if _, found := knownNodes[pvc.GetName()]; !found {
			if err := r.deletePVC(ctx, &pvc); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *attacher) diskIsAttached(ctx context.Context, volume, nodeName string) (bool, error) {
	vol, err := parseVolumeHandle(volume)
	if err != nil {
		return false, err
	}

	var node corev1.Node
	if err := a.k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return false, err
	}
	zone, found := node.GetLabels()[zoneLabel]
	if !found {
		return false, fmt.Errorf("No zone found for node %s", nodeName)
	}

	instance, err := a.computeSvc.Instances.Get(vol.project, zone, nodeName).Context(ctx).Do()
	if err != nil {
		return false, err
	}
	for _, disk := range instance.Disks {
		if disk.DeviceName == vol.name {
			return true, nil
		}
	}
	return false, nil
}

func (a *attacher) attachDisk(ctx context.Context, volume, nodeName string) error {
	vol, err := parseVolumeHandle(volume)
	if err != nil {
		return err
	}

	attach := &compute.AttachedDisk{
		DeviceName: vol.name,
		Source:     sourceFromVolumeHandle(volume),
		Mode:       "READ_WRITE",
		Type:       "PERSISTENT",
	}
	op, err := a.computeSvc.Instances.AttachDisk(vol.project, vol.zone, nodeName, attach).Context(ctx).Do()
	if err != nil {
		return err
	}
	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		pollOp, err := a.computeSvc.ZoneOperations.Get(vol.project, vol.zone, op.Name).Context(ctx).Do()
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
			return false, fmt.Errorf("error waiting for attach to %s: %v", nodeName, errs)
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("could not attach %s to %s: %w", volume, nodeName, err)
	}
	return nil
}

func parseVolumeHandle(volume string) (volumeHandle, error) {
	// example handle: projects/mattcary-gke-dev3/zones/us-central1-b/disks/pvc-eeb37e7c-faa6-4287-9114-4ee7ca9f5d0a
	parts := strings.Split(volume, "/")
	if len(parts) != 6 {
		return volumeHandle{}, fmt.Errorf("bad volume handle %s", volume)
	}
	return volumeHandle{
		project: parts[1],
		zone:    parts[3],
		name:    parts[5],
	}, nil
}

func sourceFromVolumeHandle(volume string) string {
	return "https://www.googleapis.com/compute/v1/" + volume
}
