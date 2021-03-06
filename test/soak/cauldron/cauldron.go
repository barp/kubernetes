/*
Copyright 2015 The Kubernetes Authors.

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

/*
This soak tests places a specified number of pods on each node and then
repeatedly sends queries to a service running on these pods via
a service.
*/

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/klog"
	"k8s.io/kubernetes/test/e2e/framework"
)

var (
	queriesAverage = flag.Int("queries", 100, "Number of hostname queries to make in each iteration per pod on average")
	podsPerNode    = flag.Int("pods_per_node", 1, "Number of serve_hostname pods per node")
	upTo           = flag.Int("up_to", 1, "Number of iterations or -1 for no limit")
	maxPar         = flag.Int("max_in_flight", 100, "Maximum number of queries in flight")
)

const (
	deleteTimeout          = 2 * time.Minute
	endpointTimeout        = 5 * time.Minute
	nodeListTimeout        = 2 * time.Minute
	podCreateTimeout       = 2 * time.Minute
	podStartTimeout        = 30 * time.Minute
	serviceCreateTimeout   = 2 * time.Minute
	namespaceDeleteTimeout = 5 * time.Minute
)

func main() {
	flag.Parse()

	klog.Infof("Starting cauldron soak test with queries=%d podsPerNode=%d upTo=%d maxPar=%d",
		*queriesAverage, *podsPerNode, *upTo, *maxPar)

	cc, err := restclient.InClusterConfig()
	if err != nil {
		klog.Fatalf("Failed to make client: %v", err)
	}

	client, err := clientset.NewForConfig(cc)
	if err != nil {
		klog.Fatalf("Failed to make client: %v", err)
	}

	var nodes *corev1.NodeList
	for start := time.Now(); time.Since(start) < nodeListTimeout; time.Sleep(2 * time.Second) {
		nodes, err = client.CoreV1().Nodes().List(metav1.ListOptions{})
		if err == nil {
			break
		}
		klog.Warningf("Failed to list nodes: %v", err)
	}
	if err != nil {
		klog.Fatalf("Giving up trying to list nodes: %v", err)
	}

	if len(nodes.Items) == 0 {
		klog.Fatalf("Failed to find any nodes.")
	}

	klog.Infof("Found %d nodes on this cluster:", len(nodes.Items))
	for i, node := range nodes.Items {
		klog.Infof("%d: %s", i, node.Name)
	}

	queries := *queriesAverage * len(nodes.Items) * *podsPerNode

	// Create a uniquely named namespace.
	got, err := client.CoreV1().Namespaces().Create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "serve-hostnames-"}})
	if err != nil {
		klog.Fatalf("Failed to create namespace: %v", err)
	}
	ns := got.Name
	defer func(ns string) {
		if err := client.CoreV1().Namespaces().Delete(ns, nil); err != nil {
			klog.Warningf("Failed to delete namespace %s: %v", ns, err)
		} else {
			// wait until the namespace disappears
			for i := 0; i < int(namespaceDeleteTimeout/time.Second); i++ {
				if _, err := client.CoreV1().Namespaces().Get(ns, metav1.GetOptions{}); err != nil {
					if errors.IsNotFound(err) {
						return
					}
				}
				time.Sleep(time.Second)
			}
		}
	}(ns)
	klog.Infof("Created namespace %s", ns)

	// Create a service for these pods.
	klog.Infof("Creating service %s/serve-hostnames", ns)
	// Make several attempts to create a service.
	var svc *corev1.Service
	for start := time.Now(); time.Since(start) < serviceCreateTimeout; time.Sleep(2 * time.Second) {
		t := time.Now()
		svc, err = client.CoreV1().Services(ns).Create(&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: "serve-hostnames",
				Labels: map[string]string{
					"name": "serve-hostname",
				},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{
					Protocol:   "TCP",
					Port:       9376,
					TargetPort: intstr.FromInt(9376),
				}},
				Selector: map[string]string{
					"name": "serve-hostname",
				},
			},
		})
		klog.V(4).Infof("Service create %s/server-hostnames took %v", ns, time.Since(t))
		if err == nil {
			break
		}
		klog.Warningf("After %v failed to create service %s/serve-hostnames: %v", time.Since(start), ns, err)
	}
	if err != nil {
		klog.Warningf("Unable to create service %s/%s: %v", ns, svc.Name, err)
		return
	}
	// Clean up service
	defer func() {
		klog.Infof("Cleaning up service %s/serve-hostnames", ns)
		// Make several attempts to delete the service.
		for start := time.Now(); time.Since(start) < deleteTimeout; time.Sleep(1 * time.Second) {
			if err := client.CoreV1().Services(ns).Delete(svc.Name, nil); err == nil {
				return
			}
			klog.Warningf("After %v unable to delete service %s/%s: %v", time.Since(start), ns, svc.Name, err)
		}
	}()

	// Put serve-hostname pods on each node.
	podNames := []string{}
	for i, node := range nodes.Items {
		for j := 0; j < *podsPerNode; j++ {
			podName := fmt.Sprintf("serve-hostname-%d-%d", i, j)
			podNames = append(podNames, podName)
			// Make several attempts
			for start := time.Now(); time.Since(start) < podCreateTimeout; time.Sleep(2 * time.Second) {
				klog.Infof("Creating pod %s/%s on node %s", ns, podName, node.Name)
				t := time.Now()
				_, err = client.CoreV1().Pods(ns).Create(&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: podName,
						Labels: map[string]string{
							"name": "serve-hostname",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "serve-hostname",
								Image: framework.ServeHostnameImage,
								Ports: []corev1.ContainerPort{{ContainerPort: 9376}},
							},
						},
						NodeName: node.Name,
					},
				})
				klog.V(4).Infof("Pod create %s/%s request took %v", ns, podName, time.Since(t))
				if err == nil {
					break
				}
				klog.Warningf("After %s failed to create pod %s/%s: %v", time.Since(start), ns, podName, err)
			}
			if err != nil {
				klog.Warningf("Failed to create pod %s/%s: %v", ns, podName, err)
				return
			}
		}
	}
	// Clean up the pods
	defer func() {
		klog.Info("Cleaning up pods")
		// Make several attempts to delete the pods.
		for _, podName := range podNames {
			for start := time.Now(); time.Since(start) < deleteTimeout; time.Sleep(1 * time.Second) {
				if err = client.CoreV1().Pods(ns).Delete(podName, nil); err == nil {
					break
				}
				klog.Warningf("After %v failed to delete pod %s/%s: %v", time.Since(start), ns, podName, err)
			}
		}
	}()

	klog.Info("Waiting for the serve-hostname pods to be ready")
	for _, podName := range podNames {
		var pod *corev1.Pod
		for start := time.Now(); time.Since(start) < podStartTimeout; time.Sleep(5 * time.Second) {
			pod, err = client.CoreV1().Pods(ns).Get(podName, metav1.GetOptions{})
			if err != nil {
				klog.Warningf("Get pod %s/%s failed, ignoring for %v: %v", ns, podName, err, podStartTimeout)
				continue
			}
			if pod.Status.Phase == corev1.PodRunning {
				break
			}
		}
		if pod.Status.Phase != corev1.PodRunning {
			klog.Warningf("Gave up waiting on pod %s/%s to be running (saw %v)", ns, podName, pod.Status.Phase)
		} else {
			klog.Infof("%s/%s is running", ns, podName)
		}
	}

	// Wait for the endpoints to propagate.
	for start := time.Now(); time.Since(start) < endpointTimeout; time.Sleep(10 * time.Second) {
		_, err = http.Get(fmt.Sprintf("http://serve-hostnames.%s:9376", ns))
		if err == nil {
			break
		}
		klog.Infof("After %v while making a request got error %v", time.Since(start), err)
	}
	if err != nil {
		klog.Errorf("Failed to get a response from service: %v", err)
	}

	// Repeatedly make requests.
	for iteration := 0; iteration != *upTo; iteration++ {
		responseChan := make(chan string, queries)
		// Use a channel of size *maxPar to throttle the number
		// of in-flight requests to avoid overloading the service.
		inFlight := make(chan struct{}, *maxPar)
		start := time.Now()
		for q := 0; q < queries; q++ {
			go func(i int, query int) {
				inFlight <- struct{}{}
				t := time.Now()
				resp, err := http.Get(fmt.Sprintf("http://serve-hostnames.%s:9376", ns))
				klog.V(4).Infof("Call to serve-hostnames in namespace %s took %v", ns, time.Since(t))
				if err != nil {
					klog.Warningf("Call failed during iteration %d query %d : %v", i, query, err)
					// If the query failed return a string which starts with a character
					// that can't be part of a hostname.
					responseChan <- fmt.Sprintf("!failed in iteration %d to issue query %d: %v", i, query, err)
				} else {
					defer resp.Body.Close()
					hostname, err := ioutil.ReadAll(resp.Body)
					if err != nil {
						responseChan <- fmt.Sprintf("!failed in iteration %d to read body of response: %v", i, err)
					} else {
						responseChan <- string(hostname)
					}
				}
				<-inFlight
			}(iteration, q)
		}
		responses := make(map[string]int, *podsPerNode*len(nodes.Items))
		missing := 0
		for q := 0; q < queries; q++ {
			r := <-responseChan
			klog.V(4).Infof("Got response from %s", r)
			responses[r]++
			// If the returned hostname starts with '!' then it indicates
			// an error response.
			if len(r) > 0 && r[0] == '!' {
				klog.V(3).Infof("Got response %s", r)
				missing++
			}
		}
		if missing > 0 {
			klog.Warningf("Missing %d responses out of %d", missing, queries)
		}
		// Report any nodes that did not respond.
		for n, node := range nodes.Items {
			for i := 0; i < *podsPerNode; i++ {
				name := fmt.Sprintf("serve-hostname-%d-%d", n, i)
				if _, ok := responses[name]; !ok {
					klog.Warningf("No response from pod %s on node %s at iteration %d", name, node.Name, iteration)
				}
			}
		}
		klog.Infof("Iteration %d took %v for %d queries (%.2f QPS) with %d missing",
			iteration, time.Since(start), queries-missing, float64(queries-missing)/time.Since(start).Seconds(), missing)
	}
}
