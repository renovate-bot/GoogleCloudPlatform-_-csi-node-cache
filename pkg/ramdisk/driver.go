/*
    Copyright 2023 Google LLC

    Licensed under the Apache License, Version 2.0 (the "License");
    you may not use this file except in compliance with the License.
    You may obtain a copy of the License at

        https://www.apache.org/licenses/LICENSE-2.0

    Unless required by applicable law or agreed to in writing, software
    distributed under the License is distributed on an "AS IS" BASIS,
    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
    See the License for the specific language governing permissions and
    limitations under the License.
*/

package ramdisk

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/klog"
)

// Driver is the object backing the CSI driver. It also implements identity and node services, q.v.
type Driver struct {
	endpoint  string
	nodeId    string
	ramVolume RamVolume
}

var _ csi.IdentityServer = &Driver{}
var _ csi.NodeServer = &Driver{}

// NewDriver creates a new RAM disk CSI driver using the given RamVolume.
func NewDriver(endpoint, nodeId string, ramVolume RamVolume) (*Driver, error) {
	klog.V(4).Infof("Driver: %v version: %v", driverName, driverVersion)

	d := &Driver{
		endpoint:  endpoint,
		ramVolume: ramVolume,
		nodeId:    nodeId,
	}

	return d, nil
}

// Run will serve the CSI driver. Normally this will run forever; an error will be returned otherwise.
func (d *Driver) Run() error {
	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(logGRPC),
	}
	u, err := url.Parse(d.endpoint)
	if err != nil {
		return fmt.Errorf("Cannot parse endpoint %s: %w", d.endpoint, err)
	}
	var addr string
	if u.Scheme == "unix" {
		addr = u.Path
		if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("Failed to remove %s: %w", addr, err)
		}

		listenDir := filepath.Dir(addr)
		if _, err := os.Stat(listenDir); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("Expected Kubelet plugin watcher to create parent dir %s but did not find such a dir", listenDir)
			} else {
				return fmt.Errorf("Failed to stat %s: %w", listenDir, err)
			}
		}
	} else if u.Scheme == "tcp" {
		addr = u.Host
	} else {
		return fmt.Errorf("%v endpoint scheme not supported", u.Scheme)
	}

	listener, err := net.Listen(u.Scheme, addr)
	if err != nil {
		return fmt.Errorf("Failed to listen: %w", err)
	}
	server := grpc.NewServer(opts...)
	csi.RegisterIdentityServer(server, d)
	csi.RegisterNodeServer(server, d)
	if err := server.Serve(listener); err != nil {
		return fmt.Errorf("Serving failed: %w", err)
	}
	return nil
}

func logGRPC(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	klog.V(4).Infof("%s called with request: %+v", info.FullMethod, req)
	resp, err := handler(ctx, req)
	if err != nil {
		klog.Errorf("%s returned with error: %v", info.FullMethod, err)
	} else {
		klog.V(4).Infof("%s returned with response: %+v", info.FullMethod, resp)
	}
	return resp, err
}
