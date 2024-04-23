/*
Copyright 2020 The Kubernetes Authors.

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

package queuesort

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientset "k8s.io/client-go/kubernetes"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
)

// Name is the name of the plugin used in the plugin registry and configurations.
const Name = names.PrioritySort

// PrioritySort is a plugin that implements Priority based sorting.
type PrioritySort struct {
	client clientset.Interface
}

var _ framework.QueueSortPlugin = &PrioritySort{}

// Name returns name of the plugin.
func (pl *PrioritySort) Name() string {
	return Name
}

func (pl *PrioritySort) checkMPIJob(podName string) (string, bool) {
	podNameSlice := strings.Split(podName, "-")

	if podNameSlice[len(podNameSlice)-1] == "launcher" {
		MPIJobName := strings.Join(podNameSlice[:len(podNameSlice)-1], "-")
		return MPIJobName, true
	} else if podNameSlice[len(podNameSlice)-2] == "worker" {
		MPIJobName := strings.Join(podNameSlice[:len(podNameSlice)-2], "-")
		return MPIJobName, true
	}
	return "", false
}

func (pl *PrioritySort) isMPIJobInNode(MPIJobName string) bool {
	nodes, err := pl.client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		klog.Infof("Node info error")
		return false
	}
	for _, node := range nodes.Items {
		pods, err := pl.client.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name)})
		if err != nil {
			klog.Infof("PodList load error")
			continue
		}
		for _, pod := range pods.Items {
			if pod.Namespace != "my-ns" {
				continue
			}
			runningPodIdx, isRunningPodMPIJob := pl.checkMPIJob(pod.Name)
			if isRunningPodMPIJob && runningPodIdx == MPIJobName {
				return true
			}
		}
	}
	return false
}

// Less is the function used by the activeQ heap algorithm to sort pods.
// It sorts pods based on their priority. When priorities are equal, it uses
// PodQueueInfo.timestamp.
func (pl *PrioritySort) Less(pInfo1, pInfo2 *framework.QueuedPodInfo) bool {
	p1 := corev1helpers.PodPriority(pInfo1.Pod)
	p2 := corev1helpers.PodPriority(pInfo2.Pod)
	p1MPIJobName, isP1MPIJob := pl.checkMPIJob(pInfo1.Pod.Name)
	p2MPIJobName, isP2MPIJob := pl.checkMPIJob(pInfo2.Pod.Name)

	klog.Infof("p1MPIJobName : %v, isP1MPIJob : %v", p1MPIJobName, isP1MPIJob)
	klog.Infof("p2MPIJobName : %v, isP2MPIJob : %v", p2MPIJobName, isP2MPIJob)

	if isP1MPIJob != isP2MPIJob {
		klog.Infof("QUEUEING IS OK")
		if isP1MPIJob && pl.isMPIJobInNode(p1MPIJobName) {
			return true
		} else if isP2MPIJob && pl.isMPIJobInNode(p2MPIJobName) {
			return false
		}
	}

	if _, check_1 := pInfo1.Pod.ObjectMeta.Annotations["retract-check-var"]; check_1 {
		if _, check_2 := pInfo2.Pod.ObjectMeta.Annotations["retract-check-var"]; check_2 {
			// Both Pods have been retracted
			p1Timestamp, _ := time.Parse(time.RFC3339, pInfo1.Pod.ObjectMeta.Annotations["retract-check-var"])
			p2Timestamp, _ := time.Parse(time.RFC3339, pInfo2.Pod.ObjectMeta.Annotations["retract-check-var"])
			return (p1 > p2) || (p1 == p2 && p1Timestamp.Before(p2Timestamp))
		} else {
			// Only p1 have been retracted
			p1Timestamp, _ := time.Parse(time.RFC3339, pInfo1.Pod.ObjectMeta.Annotations["retract-check-var"])
			return (p1 > p2) || (p1 == p2 && p1Timestamp.Before(pInfo2.Timestamp))
		}
	} else {
		if _, check_3 := pInfo2.Pod.ObjectMeta.Annotations["retract-check-var"]; check_3 {
			// Only p2 have been retracted
			p2Timestamp, _ := time.Parse(time.RFC3339, pInfo2.Pod.ObjectMeta.Annotations["retract-check-var"])
			return (p1 > p2) || (p1 == p2 && pInfo1.Timestamp.Before(p2Timestamp))
		} else {
			// Neither Pod has ever been retracted (Default)
			return (p1 > p2) || (p1 == p2 && pInfo1.Timestamp.Before(pInfo2.Timestamp))
		}
	}
}

// New initializes a new plugin and returns it.
func New(_ context.Context, _ runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	return &PrioritySort{}, nil
}
