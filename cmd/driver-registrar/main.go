/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"time"

	"google.golang.org/grpc"

	"github.com/golang/glog"
	"golang.org/x/sys/unix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	registerapi "k8s.io/kubernetes/pkg/kubelet/apis/pluginregistration/v1alpha1"

	"github.com/kubernetes-csi/driver-registrar/pkg/connection"
)

const (
	// Name of node annotation that contains JSON map of driver names to node
	// names
	annotationKey = "csi.volume.kubernetes.io/nodeid"

	// Default timeout of short CSI calls like GetPluginInfo
	csiTimeout = time.Second

	// Verify (and update, if needed) the node ID at this freqeuency.
	sleepDuration = 2 * time.Minute
)

// Command line flags
var (
	kubeconfig              = flag.String("kubeconfig", "", "Absolute path to the kubeconfig file. Required only when running out of cluster.")
	connectionTimeout       = flag.Duration("connection-timeout", 1*time.Minute, "Timeout for waiting for CSI driver socket.")
	csiAddress              = flag.String("csi-address", "/run/csi/socket", "Address of the CSI driver socket.")
	kubeletRegistrationPath = flag.String("kubelet-registration-path", "/var/lib/kubelet/plugins/csi-hostpath/csi.sock",
		`Enables Kubelet Plugin Registration service, and returns the specified path as "endpoint" in "PluginInfo" response.
		 If this option is set, the driver-registrar expose a unix domain socket to handle Kubelet Plugin Registration, 
		 this socket MUST be surfaced on the host in the kubelet plugin registration directory (in addition to the CSI driver socket). 
		 If plugin registration is enabled on kubelet (kubelet flag KubeletPluginsWatcher is set), then this option should be set
		 and the value should be the path of the CSI driver socket on the host machine.`)
	showVersion = flag.Bool("version", false, "Show version.")
	version     = "unknown"
	// List of supported versions
	supportedVersions = []string{"0.2.0", "0.3.0"}
)

// registrationServer is a sample plugin to work with plugin watcher
type registrationServer struct {
	driverName string
	endpoint   string
	version    []string
}

var _ registerapi.RegistrationServer = registrationServer{}

// NewregistrationServer returns an initialized registrationServer instance
func newRegistrationServer(driverName string, endpoint string, versions []string) registerapi.RegistrationServer {
	return &registrationServer{
		driverName: driverName,
		endpoint:   endpoint,
		version:    versions,
	}
}

// GetInfo is the RPC invoked by plugin watcher
func (e registrationServer) GetInfo(ctx context.Context, req *registerapi.InfoRequest) (*registerapi.PluginInfo, error) {
	glog.Infof("Received GetInfo call: %+v", req)
	return &registerapi.PluginInfo{
		Type:              registerapi.CSIPlugin,
		Name:              e.driverName,
		Endpoint:          e.endpoint,
		SupportedVersions: e.version,
	}, nil
}

func (e registrationServer) NotifyRegistrationStatus(ctx context.Context, status *registerapi.RegistrationStatus) (*registerapi.RegistrationStatusResponse, error) {
	glog.Infof("Received NotifyRegistrationStatus call: %+v", status)
	if !status.PluginRegistered {
		glog.Errorf("Registration process failed with error: %+v, restarting registration container.", status.Error)
		os.Exit(1)
	}

	return &registerapi.RegistrationStatusResponse{}, nil
}

func main() {
	flag.Set("logtostderr", "true")
	flag.Parse()

	if *showVersion {
		fmt.Println(os.Args[0], version)
		return
	}
	glog.Infof("Version: %s", version)

	// Fetch node name from environemnt variable
	k8sNodeName := os.Getenv("KUBE_NODE_NAME")
	if k8sNodeName == "" {
		glog.Error(fmt.Errorf(
			"Node name not found. The environment variable KUBE_NODE_NAME is empty."))
		os.Exit(1)
	}

	// Once https://github.com/container-storage-interface/spec/issues/159 is
	// resolved, if plugin does not support PUBLISH_UNPUBLISH_VOLUME, then we
	// can skip adding mappting to "csi.volume.kubernetes.io/nodeid" annotation.

	// Connect to CSI.
	glog.V(1).Infof("Attempting to open a gRPC connection with: %q", csiAddress)
	csiConn, err := connection.NewConnection(*csiAddress, *connectionTimeout)
	if err != nil {
		glog.Error(err.Error())
		os.Exit(1)
	}

	// Get CSI driver name.
	glog.V(1).Infof("Calling CSI driver to discover driver name.")
	ctx, cancel := context.WithTimeout(context.Background(), csiTimeout)
	defer cancel()
	csiDriverName, err := csiConn.GetDriverName(ctx)
	if err != nil {
		glog.Error(err.Error())
		os.Exit(1)
	}
	glog.V(2).Infof("CSI driver name: %q", csiDriverName)

	// Get CSI Driver Node ID
	glog.V(1).Infof("Calling CSI driver to discover node ID.")
	ctx, cancel = context.WithTimeout(context.Background(), csiTimeout)
	defer cancel()
	csiDriverNodeId, err := csiConn.NodeGetId(ctx)
	if err != nil {
		glog.Error(err.Error())
		os.Exit(1)
	}
	glog.V(2).Infof("CSI driver node ID: %q", csiDriverNodeId)

	// When kubeletRegistrationPath is specified then driver-registrar ONLY acts
	// as gRPC server which replies to registration requests initiated by kubelet's
	// pluginswatcher infrastructure. Node labeling is done by kubelet's csi code.
	if *kubeletRegistrationPath != "" {
		registrar := newRegistrationServer(csiDriverName, *kubeletRegistrationPath, supportedVersions)
		socketPath := fmt.Sprintf("/registration/%s-reg.sock", csiDriverName)
		fi, err := os.Stat(socketPath)
		if err == nil && (fi.Mode()&os.ModeSocket) != 0 {
			// Remove any socket, stale or not, but fall through for other files
			if err := os.Remove(socketPath); err != nil {
				glog.Errorf("failed to remove stale socket %s with error: %+v", socketPath, err)
				os.Exit(1)
			}
		}
		if err != nil && !os.IsNotExist(err) {
			glog.Errorf("failed to stat the socket %s with error: %+v", socketPath, err)
			os.Exit(1)
		}
		// Default to only user accessible socket, caller can open up later if desired
		oldmask := unix.Umask(0077)

		glog.Infof("Starting Registration Server at: %s\n", socketPath)
		lis, err := net.Listen("unix", socketPath)
		if err != nil {
			glog.Errorf("failed to listen on socket: %s with error: %+v", socketPath, err)
			os.Exit(1)
		}
		unix.Umask(oldmask)
		glog.Infof("Registration Server started at: %s\n", socketPath)
		grpcServer := grpc.NewServer()
		// Registers kubelet plugin watcher api.
		registerapi.RegisterRegistrationServer(grpcServer, registrar)

		// Starts service
		if err := grpcServer.Serve(lis); err != nil {
			glog.Errorf("Registration Server stopped serving: %v", err)
			os.Exit(1)
		}
		// If gRPC server is gracefully shutdown, exit
		os.Exit(0)
	}
	// Create the client config. Use kubeconfig if given, otherwise assume
	// in-cluster.
	glog.V(1).Infof("Loading kubeconfig.")
	config, err := buildConfig(*kubeconfig)
	if err != nil {
		glog.Error(err.Error())
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Error(err.Error())
		os.Exit(1)
	}

	glog.V(1).Infof("Attempt to update node annotation if needed")
	k8sNodesClient := clientset.CoreV1().Nodes()

	// Set up goroutine to cleanup (aka deregister) on termination.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		getVerifyAndDeleteNodeId(
			k8sNodeName,
			k8sNodesClient,
			csiDriverName)
		os.Exit(1)
	}()

	// This program is intended to run as a side-car container inside a
	// Kubernetes DaemonSet. Kubernetes DaemonSet only have one RestartPolicy,
	// always, meaning as soon as this container terminates, it will be started
	// again. Therefore, this program will loop indefientley and periodically
	// update the node annotation.
	// The CSI driver name and node ID are assumed to be immutable, and are not
	// refetched on subsequent loop iterations.
	for {
		getVerifyAndAddNodeId(
			k8sNodeName,
			k8sNodesClient,
			csiDriverName,
			csiDriverNodeId)
		time.Sleep(sleepDuration)
	}
}

// Fetches Kubernetes node API object corresponding to k8sNodeName.
// If the csiDriverName and csiDriverNodeId are not present in the node
// annotation, this method adds it.
func getVerifyAndAddNodeId(
	k8sNodeName string,
	k8sNodesClient corev1.NodeInterface,
	csiDriverName string,
	csiDriverNodeId string) error {
	// Add or update annotation on Node object
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Node before attempting update, so that
		// existing changes are not overwritten. RetryOnConflict uses
		// exponential backoff to avoid exhausting the apiserver.
		result, getErr := k8sNodesClient.Get(k8sNodeName, metav1.GetOptions{})
		if getErr != nil {
			glog.Errorf("Failed to get latest version of Node: %v", getErr)
			return getErr // do not wrap error
		}

		var previousAnnotationValue string
		if result.ObjectMeta.Annotations != nil {
			previousAnnotationValue =
				result.ObjectMeta.Annotations[annotationKey]
			glog.V(3).Infof(
				"previousAnnotationValue=%q", previousAnnotationValue)
		}

		existingDriverMap := map[string]string{}
		if previousAnnotationValue != "" {
			// Parse previousAnnotationValue as JSON
			if err := json.Unmarshal([]byte(previousAnnotationValue), &existingDriverMap); err != nil {
				return fmt.Errorf(
					"Failed to parse node's %q annotation value (%q) err=%v",
					annotationKey,
					previousAnnotationValue,
					err)
			}
		}

		if val, ok := existingDriverMap[csiDriverName]; ok {
			if val == csiDriverNodeId {
				// Value already exists in node annotation, nothing more to do
				glog.V(1).Infof(
					"The key value {%q: %q} alredy eixst in node %q annotation, no need to update: %v",
					csiDriverName,
					csiDriverNodeId,
					annotationKey,
					previousAnnotationValue)
				return nil
			}
		}

		// Add/update annotation value
		existingDriverMap[csiDriverName] = csiDriverNodeId
		jsonObj, err := json.Marshal(existingDriverMap)
		if err != nil {
			return fmt.Errorf(
				"Failed while trying to add key value {%q: %q} to node %q annotation. Existing value: %v",
				csiDriverName,
				csiDriverNodeId,
				annotationKey,
				previousAnnotationValue)
		}

		result.ObjectMeta.Annotations = cloneAndAddAnnotation(
			result.ObjectMeta.Annotations,
			annotationKey,
			string(jsonObj))
		_, updateErr := k8sNodesClient.Update(result)
		if updateErr == nil {
			fmt.Printf(
				"Updated node %q successfully for CSI driver %q and CSI node name %q",
				k8sNodeName,
				csiDriverName,
				csiDriverNodeId)
		}
		return updateErr // do not wrap error
	})
	if retryErr != nil {
		return fmt.Errorf("Node update failed: %v", retryErr)
	}
	return nil
}

// Fetches Kubernetes node API object corresponding to k8sNodeName.
// If the csiDriverName is present in the node annotation, it is removed.
func getVerifyAndDeleteNodeId(
	k8sNodeName string,
	k8sNodesClient corev1.NodeInterface,
	csiDriverName string) error {
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Node before attempting update, so that
		// existing changes are not overwritten. RetryOnConflict uses
		// exponential backoff to avoid exhausting the apiserver.
		result, getErr := k8sNodesClient.Get(k8sNodeName, metav1.GetOptions{})
		if getErr != nil {
			glog.Errorf("Failed to get latest version of Node: %v", getErr)
			return getErr // do not wrap error
		}

		var previousAnnotationValue string
		if result.ObjectMeta.Annotations != nil {
			previousAnnotationValue =
				result.ObjectMeta.Annotations[annotationKey]
			glog.V(3).Infof(
				"previousAnnotationValue=%q", previousAnnotationValue)
		}

		existingDriverMap := map[string]string{}
		if previousAnnotationValue == "" {
			// Value already exists in node annotation, nothing more to do
			glog.V(1).Infof(
				"The key %q does not exist in node %q annotation, no need to cleanup.",
				csiDriverName,
				annotationKey)
			return nil
		}

		// Parse previousAnnotationValue as JSON
		if err := json.Unmarshal([]byte(previousAnnotationValue), &existingDriverMap); err != nil {
			return fmt.Errorf(
				"Failed to parse node's %q annotation value (%q) err=%v",
				annotationKey,
				previousAnnotationValue,
				err)
		}

		if _, ok := existingDriverMap[csiDriverName]; !ok {
			// Value already exists in node annotation, nothing more to do
			glog.V(1).Infof(
				"The key %q does not eixst in node %q annotation, no need to cleanup: %v",
				csiDriverName,
				annotationKey,
				previousAnnotationValue)
			return nil
		}

		// Add/update annotation value
		delete(existingDriverMap, csiDriverName)
		jsonObj, err := json.Marshal(existingDriverMap)
		if err != nil {
			return fmt.Errorf(
				"Failed while trying to remove key %q from node %q annotation. Existing data: %v",
				csiDriverName,
				annotationKey,
				previousAnnotationValue)
		}

		result.ObjectMeta.Annotations = cloneAndAddAnnotation(
			result.ObjectMeta.Annotations,
			annotationKey,
			string(jsonObj))
		_, updateErr := k8sNodesClient.Update(result)
		if updateErr == nil {
			fmt.Printf(
				"Updated node %q annotation to remove CSI driver %q.",
				k8sNodeName,
				csiDriverName)
		}
		return updateErr // do not wrap error
	})
	if retryErr != nil {
		return fmt.Errorf("Node update failed: %v", retryErr)
	}
	return nil
}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	// Return config object which uses the service account kubernetes gives to
	// pods. It's intended for clients that are running inside a pod running on
	// kubernetes.
	return rest.InClusterConfig()
}

// Clones the given map and returns a new map with the given key and value added.
// Returns the given map, if annotationKey is empty.
func cloneAndAddAnnotation(
	annotations map[string]string,
	annotationKey,
	annotationValue string) map[string]string {
	if annotationKey == "" {
		// Don't need to add an annotation.
		return annotations
	}
	// Clone.
	newAnnotations := map[string]string{}
	for key, value := range annotations {
		newAnnotations[key] = value
	}
	newAnnotations[annotationKey] = annotationValue
	return newAnnotations
}
