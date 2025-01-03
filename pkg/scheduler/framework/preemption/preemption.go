/*
Copyright 2021 The Kubernetes Authors.

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

package preemption

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic"
	corelisters "k8s.io/client-go/listers/core/v1"
	policylisters "k8s.io/client-go/listers/policy/v1"
	"k8s.io/client-go/tools/clientcmd"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	apipod "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/parallelize"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/util"
)

// Candidate represents a nominated node on which the preemptor can be scheduled,
// along with the list of victims that should be evicted for the preemptor to fit the node.
type Candidate interface {
	// Victims wraps a list of to-be-preempted Pods and the number of PDB violation.
	Victims() *extenderv1.Victims
	// Name returns the target node name where the preemptor gets nominated to run.
	Name() string
}

type candidate struct {
	victims *extenderv1.Victims
	name    string
}

// Victims returns s.victims.
func (s *candidate) Victims() *extenderv1.Victims {
	return s.victims
}

// Name returns s.name.
func (s *candidate) Name() string {
	return s.name
}

type candidateList struct {
	idx   int32
	items []Candidate
}

func newCandidateList(size int32) *candidateList {
	return &candidateList{idx: -1, items: make([]Candidate, size)}
}

// add adds a new candidate to the internal array atomically.
func (cl *candidateList) add(c *candidate) {
	if idx := atomic.AddInt32(&cl.idx, 1); idx < int32(len(cl.items)) {
		cl.items[idx] = c
	}
}

// size returns the number of candidate stored. Note that some add() operations
// might still be executing when this is called, so care must be taken to
// ensure that all add() operations complete before accessing the elements of
// the list.
func (cl *candidateList) size() int32 {
	n := atomic.LoadInt32(&cl.idx) + 1
	if n >= int32(len(cl.items)) {
		n = int32(len(cl.items))
	}
	return n
}

// get returns the internal candidate array. This function is NOT atomic and
// assumes that all add() operations have been completed.
func (cl *candidateList) get() []Candidate {
	return cl.items[:cl.size()]
}

// Interface is expected to be implemented by different preemption plugins as all those member
// methods might have different behavior compared with the default preemption.
type Interface interface {
	// GetOffsetAndNumCandidates chooses a random offset and calculates the number of candidates that should be
	// shortlisted for dry running preemption.
	GetOffsetAndNumCandidates(nodes int32) (int32, int32)
	// CandidatesToVictimsMap builds a map from the target node to a list of to-be-preempted Pods and the number of PDB violation.
	CandidatesToVictimsMap(candidates []Candidate) map[string]*extenderv1.Victims
	// PodEligibleToPreemptOthers returns one bool and one string. The bool indicates whether this pod should be considered for
	// preempting other pods or not. The string includes the reason if this pod isn't eligible.
	PodEligibleToPreemptOthers(pod *v1.Pod, nominatedNodeStatus *framework.Status) (bool, string)
	// SelectVictimsOnNode finds minimum set of pods on the given node that should be preempted in order to make enough room
	// for "pod" to be scheduled.
	// Note that both `state` and `nodeInfo` are deep copied.
	SelectVictimsOnNode(ctx context.Context, state *framework.CycleState,
		pod *v1.Pod, nodeInfo *framework.NodeInfo, pdbs []*policy.PodDisruptionBudget) ([]*v1.Pod, int, *framework.Status)
	// OrderedScoreFuncs returns a list of ordered score functions to select preferable node where victims will be preempted.
	// The ordered score functions will be processed one by one iff we find more than one node with the highest score.
	// Default score functions will be processed if nil returned here for backwards-compatibility.
	OrderedScoreFuncs(ctx context.Context, nodesToVictims map[string]*extenderv1.Victims) []func(node string) int64
}

type Evaluator struct {
	PluginName string
	Handler    framework.Handle
	PodLister  corelisters.PodLister
	PdbLister  policylisters.PodDisruptionBudgetLister
	State      *framework.CycleState
	Interface
}

func (ev *Evaluator) Dynamic(ctx context.Context, pod *v1.Pod, m framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
	scalableModelData := make(map[string][]float64)
	scalableModelData["VGG16"] = []float64{4.232, 3.499, 4.61, 4.526, 5.396, 6.363}
	scalableModelData["VGG19"] = []float64{3.693, 2.793, 3.6294, 4.3105, 5.2051, 5.934}
	scalableModelData["inceptionv3"] = []float64{3.5, 5.495, 6.29, 8.449, 9.693, 11.262}
	scalableModelData["alex-net"] = []float64{15.969, 7.496, 9.002, 10.572, 13.005, 14.708}
	scalableModelData["resnet50"] = []float64{6.78098, 7.777, 12.4701, 12.18717, 15.90254, 18.968}
	scalableModelData["resnet101"] = []float64{4.253, 5.519, 8.863, 8.8171, 9.686, 12.904}
	var requestGPUs int64

	nodes, err := ev.Handler.ClientSet().CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Infof("Node info error")
	}

	for _, container := range pod.Spec.Containers {
		if gpuRequest, ok := container.Resources.Requests["nvidia.com/gpu"]; ok {
			requestGPUs = int64(gpuRequest.Value())
		}
	}
	runningMPIJobs := ev.ListRunningMPIJob(ctx, nodes)
	if err != nil {
		klog.Fatalf("Failed to list MPIJobs: %s", err.Error())
	}
	idleGPUs := ev.idleGPUsinNodes(ctx, nodes)
	resourceRetrieve, allocatableNodeName := ev.Retrieve(ctx, pod, scalableModelData, nodes, idleGPUs, requestGPUs, runningMPIJobs)

	if resourceRetrieve {
		return framework.NewPostFilterResultWithNominatedNode(allocatableNodeName), framework.NewStatus(framework.Success)
	}

	if idleGPUs == 0 {
		return framework.NewPostFilterResultWithNominatedNode(""), framework.NewStatus(framework.Unschedulable, "Nothing can do")
	}

	var scaleOutMPIJobName string
	maxThroughput := 0.0
	for _, MPIJobName := range runningMPIJobs {
		MPIJob, err := ev.GetMPIJob(ctx, "my-ns", MPIJobName)
		if err != nil {
			klog.Infof("Failed to get MPIJob: %v", err)
		}

		annotations, found, err := unstructured.NestedStringMap(MPIJob.Object, "metadata", "annotations")
		if err != nil {
			klog.Infof("Error loading annotations: %v", err)
		}
		if !found {
			klog.Infof("Error finding annotations: %v", err)
		}

		usingGPUs, found, err := unstructured.NestedInt64(MPIJob.Object, "spec", "mpiReplicaSpecs", "Worker", "replicas")
		if err != nil {
			klog.Infof("Error reading replicas: %v", err)
		}
		if !found {
			klog.Infof("Replicas not found")
		}
		usingGPUs += 1
		if usingGPUs > 5 {
			continue
		}
		if maxThroughput < (scalableModelData[annotations["model-name"]][usingGPUs] - scalableModelData[annotations["model-name"]][usingGPUs-1]) {
			if _, check := annotations["scale-out"]; !check {
				maxThroughput = scalableModelData[annotations["model-name"]][usingGPUs] - scalableModelData[annotations["model-name"]][usingGPUs-1]
				scaleOutMPIJobName = MPIJobName
			}
		}
	}
	if maxThroughput > 0.0 {
		ev.MPIJobScaling(ctx, "my-ns", scaleOutMPIJobName, 1)
	}
	return framework.NewPostFilterResultWithNominatedNode(""), framework.NewStatus(framework.Unschedulable, "Scale-Out MPIJob")
}

func getPodTimestamp(pod *v1.Pod) metav1.Time {
	if timestampStr, ok := pod.ObjectMeta.Annotations["retract-check-var"]; ok {
		if timestamp, err := time.Parse(time.RFC3339, timestampStr); err == nil {
			return metav1.Time{Time: timestamp}
		}
	}
	return pod.ObjectMeta.CreationTimestamp
}

func (ev *Evaluator) Retrieve(ctx context.Context, podNow *v1.Pod, scalableModelData map[string][]float64, nodes *v1.NodeList, idleGPUs int64, requestGPUs int64, runningMPIJobs []string) (bool, string) {
	var backfilledPods []v1.Pod
	var scaleOutMPIJobs []*unstructured.Unstructured
	var retrieveCandidates [][]int
	for _, node := range nodes.Items {
		pods, err := ev.Handler.ClientSet().CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name)})
		if err != nil {
			klog.Infof("PodList load error")
			continue
		}
		for _, pod := range pods.Items {
			if schedStateOfPod, check := pod.Annotations["scheduling-state"]; check && schedStateOfPod == "backfilled" {
				podNowCreationTimeStamp := getPodTimestamp(podNow)
				podCreationTimeStamp := getPodTimestamp(&pod)

				if podNowCreationTimeStamp.Before(&podCreationTimeStamp) {
					backfilledPods = append(backfilledPods, pod)
				}
			}
		}
	}
	for _, MPIJobName := range runningMPIJobs {
		MPIJob, err := ev.GetMPIJob(ctx, "my-ns", MPIJobName)
		if err != nil {
			klog.Infof("Failed to get MPIJob: %v", err)
		}

		annotations, found, err := unstructured.NestedStringMap(MPIJob.Object, "metadata", "annotations")
		if err != nil {
			klog.Infof("Error reading replicas: %v", err)
		}
		if !found {
			klog.Infof("Replicas not found")
		}

		if _, check := annotations["scale-out"]; check {
			scaleOutMPIJobs = append(scaleOutMPIJobs, MPIJob)
		}
	}

	for i := 0; i < len(backfilledPods); i++ {
		var backfilledInfo []int
		var gpuRequest resource.Quantity
		backfilledInfo = append(backfilledInfo, 0)
		backfilledInfo = append(backfilledInfo, i)
		for _, container := range backfilledPods[i].Spec.Containers {
			gpuRequest = container.Resources.Requests["nvidia.com/gpu"]
		}
		backfilledInfo = append(backfilledInfo, int(gpuRequest.Value()))
		backfilledInfo = append(backfilledInfo, int(scalableModelData[backfilledPods[i].Annotations["model-name"]][int(gpuRequest.Value())]))
		retrieveCandidates = append(retrieveCandidates, backfilledInfo)
	}
	for i := 0; i < len(scaleOutMPIJobs); i++ {
		var scaleInfo []int
		scaleInfo = append(scaleInfo, 1)
		scaleInfo = append(scaleInfo, i)

		replicas, found, err := unstructured.NestedInt64(scaleOutMPIJobs[i].Object, "spec", "mpiReplicaSpecs", "Worker", "replicas")
		if err != nil {
			log.Fatalf("Error reading replicas: %s", err.Error())
		}
		if !found {
			log.Fatalf("Replicas not found")
		}
		annotations, found, err := unstructured.NestedStringMap(scaleOutMPIJobs[i].Object, "metadata", "annotations")
		if err != nil {
			klog.Infof("Error reading replicas: %v", err)
		}
		if !found {
			klog.Infof("Replicas not found")
		}
		scaleOutGpus, _ := strconv.Atoi(annotations["scale-out"])

		s1 := scalableModelData[annotations["model-name"]][int(replicas)]
		s2 := scalableModelData[annotations["model-name"]][int(replicas)-scaleOutGpus]

		scaleInfo = append(scaleInfo, scaleOutGpus)
		scaleInfo = append(scaleInfo, int(s1-s2))
		retrieveCandidates = append(retrieveCandidates, scaleInfo)
	}

	if len(retrieveCandidates) == 0 {
		return false, ""
	}

	needGPUs := int(requestGPUs - idleGPUs)
	sort.SliceStable(retrieveCandidates, func(i, j int) bool {
		return retrieveCandidates[i][3] < retrieveCandidates[j][3]
	})
	sort.SliceStable(retrieveCandidates, func(i, j int) bool {
		return retrieveCandidates[i][2] < retrieveCandidates[j][2]
	})

	checkTemp := false
	needGPUsTemp := needGPUs
	for _, candidate := range retrieveCandidates {
		needGPUsTemp -= candidate[2]
		if needGPUsTemp <= 0 {
			checkTemp = true
			break
		}
	}

	if !checkTemp {
		return false, ""
	} else {
		needGPUsTemp = needGPUs
		for _, candidate := range retrieveCandidates {
			if candidate[0] == 0 {
				err := util.RetractPod(ctx, ev.Handler.ClientSet(), &backfilledPods[candidate[1]])
				if err != nil {
					klog.Infof("Fail retract: %v", err)
				}
			} else {
				ev.MPIJobScaling(ctx, "my-ns", "tensorflow-mnist-elastic", -int64(candidate[2]))
			}
			needGPUsTemp -= candidate[2]
			if needGPUsTemp <= 0 {
				return true, ""
			}
		}
	}
	return false, ""
}

func Contains(slice []string, target string) bool {
	for _, item := range slice {
		if item == target {
			return true
		}
	}
	return false
}

func (ev *Evaluator) idleGPUsinNodes(ctx context.Context, nodes *v1.NodeList) int64 {
	capacityGPUcount := 0
	allocatedGPUcount := 0
	for _, node := range nodes.Items {
		if val, ok := node.Status.Capacity["nvidia.com/gpu"]; ok {
			capacityGPUcount += int(val.Value())
		}
		pods, err := ev.Handler.ClientSet().CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name)})
		if err != nil {
			klog.Infof("Pod loading error")
			continue
		}
		for _, pod := range pods.Items {
			for _, container := range pod.Spec.Containers {
				if gpuRequest, ok := container.Resources.Requests["nvidia.com/gpu"]; ok {
					allocatedGPUcount += int(gpuRequest.Value())
				}
			}
		}
	}
	return int64(capacityGPUcount - allocatedGPUcount)
}

func (ev *Evaluator) GetMPIJob(ctx context.Context, ns string, MPIJobName string) (*unstructured.Unstructured, error) {
	config, err := clientcmd.BuildConfigFromFlags("", "/etc/kubernetes/scheduler.conf")
	if err != nil {
		klog.Infof("Failed to get in-cluster config: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		klog.Infof("Failed to create dynamic client: %v", err)
	}
	gvr := schema.GroupVersionResource{
		Group:    "kubeflow.org",
		Version:  "v1",
		Resource: "mpijobs",
	}
	MPIJob, err := dynamicClient.Resource(gvr).Namespace(ns).Get(ctx, MPIJobName, metav1.GetOptions{})
	return MPIJob, err
}

func (ev *Evaluator) ListRunningMPIJob(ctx context.Context, nodes *v1.NodeList) []string {
	var ListofRunningMPIJob []string
	for _, node := range nodes.Items {
		pods, err := ev.Handler.ClientSet().CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name)})
		if err != nil {
			klog.Infof("Pod load error")
			continue
		}
		for _, pod := range pods.Items {
			podNameSlice := strings.Split(pod.Name, "-")
			if len(podNameSlice) < 2 {
				continue
			}

			if podNameSlice[len(podNameSlice)-1] == "launcher" && podNameSlice[len(podNameSlice)-2] == "elastic" {
				MPIJobName := strings.Join(podNameSlice[:len(podNameSlice)-1], "-")
				if !Contains(ListofRunningMPIJob, MPIJobName) {
					ListofRunningMPIJob = append(ListofRunningMPIJob, MPIJobName)
				}
			} else if podNameSlice[len(podNameSlice)-2] == "worker" && podNameSlice[len(podNameSlice)-3] == "elastic" {
				MPIJobName := strings.Join(podNameSlice[:len(podNameSlice)-2], "-")
				if !Contains(ListofRunningMPIJob, MPIJobName) {
					ListofRunningMPIJob = append(ListofRunningMPIJob, MPIJobName)
				}
			}
		}
	}
	return ListofRunningMPIJob
}

func (ev *Evaluator) MPIJobScaling(ctx context.Context, ns string, MPIJobName string, scaleNum int64) {
	config, err := clientcmd.BuildConfigFromFlags("", "/etc/kubernetes/scheduler.conf")
	if err != nil {
		klog.Infof("Failed to get in-cluster config: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		klog.Infof("Failed to create dynamic client: %v", err)
	}
	gvr := schema.GroupVersionResource{
		Group:    "kubeflow.org",
		Version:  "v1",
		Resource: "mpijobs",
	}
	MPIJob, err := dynamicClient.Resource(gvr).Namespace(ns).Get(ctx, MPIJobName, metav1.GetOptions{})
	if err != nil {
		klog.Infof("Failed to list MPIJobs: %v", err)
	}

	nowGPUcount, found, err := unstructured.NestedInt64(MPIJob.Object, "spec", "mpiReplicaSpecs", "Worker", "replicas")
	if err != nil {
		klog.Infof("Error reading replicas: %v", err)
	}
	if !found {
		klog.Infof("Replicas not found")
	}

	workerReplicasPath := []string{"spec", "mpiReplicaSpecs", "Worker", "replicas"}
	if err := unstructured.SetNestedField(MPIJob.Object, int64(nowGPUcount+scaleNum), workerReplicasPath...); err != nil {
		klog.Infof("Failed to set replicas: %v", err)
	}

	if scaleNum > 0 {
		annotations, found, err := unstructured.NestedStringMap(MPIJob.Object, "metadata", "annotations")
		if err != nil {
			klog.Infof("Error loading annotations: %v", err)
			return
		}
		if !found {
			annotations = make(map[string]string)
			annotations["scale-out"] = strconv.Itoa(int(scaleNum))
		} else {
			annotations["scale-out"] += strconv.Itoa(int(scaleNum))
		}
		if err := unstructured.SetNestedStringMap(MPIJob.Object, annotations, "metadata", "annotations"); err != nil {
			klog.Infof("Failed to set annotations: %v", err)
		}
	}
	updatedMPIJob, err := dynamicClient.Resource(gvr).Namespace(ns).Update(ctx, MPIJob, metav1.UpdateOptions{})
	if err != nil {
		klog.Infof("Failed to update MPIJob: %v", err)
	}
	klog.Infof("Updated Info : %v", updatedMPIJob.Object)
}

// Preempt returns a PostFilterResult carrying suggested nominatedNodeName, along with a Status.
// The semantics of returned <PostFilterResult, Status> varies on different scenarios:
//
//   - <nil, Error>. This denotes it's a transient/rare error that may be self-healed in future cycles.
//
//   - <nil, Unschedulable>. This status is mostly as expected like the preemptor is waiting for the
//     victims to be fully terminated.
//
//   - In both cases above, a nil PostFilterResult is returned to keep the pod's nominatedNodeName unchanged.
//
//   - <non-nil PostFilterResult, Unschedulable>. It indicates the pod cannot be scheduled even with preemption.
//     In this case, a non-nil PostFilterResult is returned and result.NominatingMode instructs how to deal with
//     the nominatedNodeName.
//
//   - <non-nil PostFilterResult, Success>. It's the regular happy path
//     and the non-empty nominatedNodeName will be applied to the preemptor pod.
func (ev *Evaluator) Preempt(ctx context.Context, pod *v1.Pod, m framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
	logger := klog.FromContext(ctx)

	// 0) Fetch the latest version of <pod>.
	// It's safe to directly fetch pod here. Because the informer cache has already been
	// initialized when creating the Scheduler obj.
	// However, tests may need to manually initialize the shared pod informer.
	podNamespace, podName := pod.Namespace, pod.Name
	pod, err := ev.PodLister.Pods(pod.Namespace).Get(pod.Name)
	if err != nil {
		logger.Error(err, "Could not get the updated preemptor pod object", "pod", klog.KRef(podNamespace, podName))
		return nil, framework.AsStatus(err)
	}

	// 1) Ensure the preemptor is eligible to preempt other pods.
	if ok, msg := ev.PodEligibleToPreemptOthers(pod, m[pod.Status.NominatedNodeName]); !ok {
		logger.V(5).Info("Pod is not eligible for preemption", "pod", klog.KObj(pod), "reason", msg)
		return nil, framework.NewStatus(framework.Unschedulable, msg)
	}

	// 2) Find all preemption candidates.
	candidates, nodeToStatusMap, err := ev.findCandidates(ctx, pod, m)
	if err != nil && len(candidates) == 0 {
		return nil, framework.AsStatus(err)
	}

	// Return a FitError only when there are no candidates that fit the pod.
	if len(candidates) == 0 {
		fitError := &framework.FitError{
			Pod:         pod,
			NumAllNodes: len(nodeToStatusMap),
			Diagnosis: framework.Diagnosis{
				NodeToStatusMap: nodeToStatusMap,
				// Leave UnschedulablePlugins or PendingPlugins as nil as it won't be used on moving Pods.
			},
		}
		// Specify nominatedNodeName to clear the pod's nominatedNodeName status, if applicable.
		return framework.NewPostFilterResultWithNominatedNode(""), framework.NewStatus(framework.Unschedulable, fitError.Error())
	}

	// 3) Interact with registered Extenders to filter out some candidates if needed.
	candidates, status := ev.callExtenders(logger, pod, candidates)
	if !status.IsSuccess() {
		return nil, status
	}

	// 4) Find the best candidate.
	bestCandidate := ev.SelectCandidate(ctx, candidates)
	if bestCandidate == nil || len(bestCandidate.Name()) == 0 {
		return nil, framework.NewStatus(framework.Unschedulable, "no candidate node for preemption")
	}

	// 5) Perform preparation work before nominating the selected candidate.
	if status := ev.prepareCandidate(ctx, bestCandidate, pod, ev.PluginName); !status.IsSuccess() {
		return nil, status
	}

	return framework.NewPostFilterResultWithNominatedNode(bestCandidate.Name()), framework.NewStatus(framework.Success)
}

// FindCandidates calculates a slice of preemption candidates.
// Each candidate is executable to make the given <pod> schedulable.
func (ev *Evaluator) findCandidates(ctx context.Context, pod *v1.Pod, m framework.NodeToStatusMap) ([]Candidate, framework.NodeToStatusMap, error) {
	allNodes, err := ev.Handler.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, nil, err
	}
	if len(allNodes) == 0 {
		return nil, nil, errors.New("no nodes available")
	}
	logger := klog.FromContext(ctx)
	potentialNodes, unschedulableNodeStatus := nodesWherePreemptionMightHelp(allNodes, m)
	if len(potentialNodes) == 0 {
		logger.V(3).Info("Preemption will not help schedule pod on any node", "pod", klog.KObj(pod))
		// In this case, we should clean-up any existing nominated node name of the pod.
		if err := util.ClearNominatedNodeName(ctx, ev.Handler.ClientSet(), pod); err != nil {
			logger.Error(err, "Could not clear the nominatedNodeName field of pod", "pod", klog.KObj(pod))
			// We do not return as this error is not critical.
		}
		return nil, unschedulableNodeStatus, nil
	}

	pdbs, err := getPodDisruptionBudgets(ev.PdbLister)
	if err != nil {
		return nil, nil, err
	}

	offset, numCandidates := ev.GetOffsetAndNumCandidates(int32(len(potentialNodes)))
	if loggerV := logger.V(5); logger.Enabled() {
		var sample []string
		for i := offset; i < offset+10 && i < int32(len(potentialNodes)); i++ {
			sample = append(sample, potentialNodes[i].Node().Name)
		}
		loggerV.Info("Selected candidates from a pool of nodes", "potentialNodesCount", len(potentialNodes), "offset", offset, "sampleLength", len(sample), "sample", sample, "candidates", numCandidates)
	}
	candidates, nodeStatuses, err := ev.DryRunPreemption(ctx, pod, potentialNodes, pdbs, offset, numCandidates)
	for node, nodeStatus := range unschedulableNodeStatus {
		nodeStatuses[node] = nodeStatus
	}
	return candidates, nodeStatuses, err
}

// callExtenders calls given <extenders> to select the list of feasible candidates.
// We will only check <candidates> with extenders that support preemption.
// Extenders which do not support preemption may later prevent preemptor from being scheduled on the nominated
// node. In that case, scheduler will find a different host for the preemptor in subsequent scheduling cycles.
func (ev *Evaluator) callExtenders(logger klog.Logger, pod *v1.Pod, candidates []Candidate) ([]Candidate, *framework.Status) {
	extenders := ev.Handler.Extenders()
	nodeLister := ev.Handler.SnapshotSharedLister().NodeInfos()
	if len(extenders) == 0 {
		return candidates, nil
	}

	// Migrate candidate slice to victimsMap to adapt to the Extender interface.
	// It's only applicable for candidate slice that have unique nominated node name.
	victimsMap := ev.CandidatesToVictimsMap(candidates)
	if len(victimsMap) == 0 {
		return candidates, nil
	}
	for _, extender := range extenders {
		if !extender.SupportsPreemption() || !extender.IsInterested(pod) {
			continue
		}
		nodeNameToVictims, err := extender.ProcessPreemption(pod, victimsMap, nodeLister)
		if err != nil {
			if extender.IsIgnorable() {
				logger.Info("Skipped extender as it returned error and has ignorable flag set",
					"extender", extender.Name(), "err", err)
				continue
			}
			return nil, framework.AsStatus(err)
		}
		// Check if the returned victims are valid.
		for nodeName, victims := range nodeNameToVictims {
			if victims == nil || len(victims.Pods) == 0 {
				if extender.IsIgnorable() {
					delete(nodeNameToVictims, nodeName)
					logger.Info("Ignored node for which the extender didn't report victims", "node", klog.KRef("", nodeName), "extender", extender.Name())
					continue
				}
				return nil, framework.AsStatus(fmt.Errorf("expected at least one victim pod on node %q", nodeName))
			}
		}

		// Replace victimsMap with new result after preemption. So the
		// rest of extenders can continue use it as parameter.
		victimsMap = nodeNameToVictims

		// If node list becomes empty, no preemption can happen regardless of other extenders.
		if len(victimsMap) == 0 {
			break
		}
	}

	var newCandidates []Candidate
	for nodeName := range victimsMap {
		newCandidates = append(newCandidates, &candidate{
			victims: victimsMap[nodeName],
			name:    nodeName,
		})
	}
	return newCandidates, nil
}

// SelectCandidate chooses the best-fit candidate from given <candidates> and return it.
// NOTE: This method is exported for easier testing in default preemption.
func (ev *Evaluator) SelectCandidate(ctx context.Context, candidates []Candidate) Candidate {
	logger := klog.FromContext(ctx)

	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	victimsMap := ev.CandidatesToVictimsMap(candidates)
	scoreFuncs := ev.OrderedScoreFuncs(ctx, victimsMap)
	candidateNode := pickOneNodeForPreemption(logger, victimsMap, scoreFuncs)

	// Same as candidatesToVictimsMap, this logic is not applicable for out-of-tree
	// preemption plugins that exercise different candidates on the same nominated node.
	if victims := victimsMap[candidateNode]; victims != nil {
		return &candidate{
			victims: victims,
			name:    candidateNode,
		}
	}

	// We shouldn't reach here.
	logger.Error(errors.New("no candidate selected"), "Should not reach here", "candidates", candidates)
	// To not break the whole flow, return the first candidate.
	return candidates[0]
}

// prepareCandidate does some preparation work before nominating the selected candidate:
// - Evict the victim pods
// - Reject the victim pods if they are in waitingPod map
// - Clear the low-priority pods' nominatedNodeName status if needed
func (ev *Evaluator) prepareCandidate(ctx context.Context, c Candidate, pod *v1.Pod, pluginName string) *framework.Status {
	fh := ev.Handler
	cs := ev.Handler.ClientSet()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	logger := klog.FromContext(ctx)
	errCh := parallelize.NewErrorChannel()
	preemptPod := func(index int) {
		victim := c.Victims().Pods[index]
		// If the victim is a WaitingPod, send a reject message to the PermitPlugin.
		// Otherwise we should delete the victim.
		if waitingPod := fh.GetWaitingPod(victim.UID); waitingPod != nil {
			waitingPod.Reject(pluginName, "preempted")
			logger.V(2).Info("Preemptor pod rejected a waiting pod", "preemptor", klog.KObj(pod), "waitingPod", klog.KObj(victim), "node", c.Name())
		} else {
			if feature.DefaultFeatureGate.Enabled(features.PodDisruptionConditions) {
				condition := &v1.PodCondition{
					Type:    v1.DisruptionTarget,
					Status:  v1.ConditionTrue,
					Reason:  v1.PodReasonPreemptionByScheduler,
					Message: fmt.Sprintf("%s: preempting to accommodate a higher priority pod", pod.Spec.SchedulerName),
				}
				newStatus := pod.Status.DeepCopy()
				updated := apipod.UpdatePodCondition(newStatus, condition)
				if updated {
					if err := util.PatchPodStatus(ctx, cs, victim, newStatus); err != nil {
						logger.Error(err, "Could not add DisruptionTarget condition due to preemption", "pod", klog.KObj(victim), "preemptor", klog.KObj(pod))
						errCh.SendErrorWithCancel(err, cancel)
						return
					}
				}
			}
			// RetractPod
			if err := util.DeletePod(ctx, cs, victim); err != nil {
				logger.Error(err, "Preempted pod", "pod", klog.KObj(victim), "preemptor", klog.KObj(pod))
				errCh.SendErrorWithCancel(err, cancel)
				return
			}
			logger.V(2).Info("Preemptor Pod preempted victim Pod", "preemptor", klog.KObj(pod), "victim", klog.KObj(victim), "node", c.Name())
		}

		fh.EventRecorder().Eventf(victim, pod, v1.EventTypeNormal, "Preempted", "Preempting", "Preempted by pod %v on node %v", pod.UID, c.Name())
	}

	fh.Parallelizer().Until(ctx, len(c.Victims().Pods), preemptPod, ev.PluginName)
	if err := errCh.ReceiveError(); err != nil {
		return framework.AsStatus(err)
	}

	metrics.PreemptionVictims.Observe(float64(len(c.Victims().Pods)))

	// Lower priority pods nominated to run on this node, may no longer fit on
	// this node. So, we should remove their nomination. Removing their
	// nomination updates these pods and moves them to the active queue. It
	// lets scheduler find another place for them.
	nominatedPods := getLowerPriorityNominatedPods(logger, fh, pod, c.Name())
	if err := util.ClearNominatedNodeName(ctx, cs, nominatedPods...); err != nil {
		logger.Error(err, "Cannot clear 'NominatedNodeName' field")
		// We do not return as this error is not critical.
	}

	return nil
}

// nodesWherePreemptionMightHelp returns a list of nodes with failed predicates
// that may be satisfied by removing pods from the node.
func nodesWherePreemptionMightHelp(nodes []*framework.NodeInfo, m framework.NodeToStatusMap) ([]*framework.NodeInfo, framework.NodeToStatusMap) {
	var potentialNodes []*framework.NodeInfo
	nodeStatuses := make(framework.NodeToStatusMap)
	for _, node := range nodes {
		name := node.Node().Name
		// We rely on the status by each plugin - 'Unschedulable' or 'UnschedulableAndUnresolvable'
		// to determine whether preemption may help or not on the node.
		if m[name].Code() == framework.UnschedulableAndUnresolvable {
			nodeStatuses[node.Node().Name] = framework.NewStatus(framework.UnschedulableAndUnresolvable, "Preemption is not helpful for scheduling")
			continue
		}
		potentialNodes = append(potentialNodes, node)
	}
	return potentialNodes, nodeStatuses
}

func getPodDisruptionBudgets(pdbLister policylisters.PodDisruptionBudgetLister) ([]*policy.PodDisruptionBudget, error) {
	if pdbLister != nil {
		return pdbLister.List(labels.Everything())
	}
	return nil, nil
}

// pickOneNodeForPreemption chooses one node among the given nodes.
// It assumes pods in each map entry are ordered by decreasing priority.
// If the scoreFuns is not empty, It picks a node based on score scoreFuns returns.
// If the scoreFuns is empty,
// It picks a node based on the following criteria:
// 1. A node with minimum number of PDB violations.
// 2. A node with minimum highest priority victim is picked.
// 3. Ties are broken by sum of priorities of all victims.
// 4. If there are still ties, node with the minimum number of victims is picked.
// 5. If there are still ties, node with the latest start time of all highest priority victims is picked.
// 6. If there are still ties, the first such node is picked (sort of randomly).
// The 'minNodes1' and 'minNodes2' are being reused here to save the memory
// allocation and garbage collection time.
func pickOneNodeForPreemption(logger klog.Logger, nodesToVictims map[string]*extenderv1.Victims, scoreFuncs []func(node string) int64) string {
	if len(nodesToVictims) == 0 {
		return ""
	}

	allCandidates := make([]string, 0, len(nodesToVictims))
	for node := range nodesToVictims {
		allCandidates = append(allCandidates, node)
	}

	if len(scoreFuncs) == 0 {
		minNumPDBViolatingScoreFunc := func(node string) int64 {
			// The smaller the NumPDBViolations, the higher the score.
			return -nodesToVictims[node].NumPDBViolations
		}
		minHighestPriorityScoreFunc := func(node string) int64 {
			// highestPodPriority is the highest priority among the victims on this node.
			highestPodPriority := corev1helpers.PodPriority(nodesToVictims[node].Pods[0])
			// The smaller the highestPodPriority, the higher the score.
			return -int64(highestPodPriority)
		}
		minSumPrioritiesScoreFunc := func(node string) int64 {
			var sumPriorities int64
			for _, pod := range nodesToVictims[node].Pods {
				// We add MaxInt32+1 to all priorities to make all of them >= 0. This is
				// needed so that a node with a few pods with negative priority is not
				// picked over a node with a smaller number of pods with the same negative
				// priority (and similar scenarios).
				sumPriorities += int64(corev1helpers.PodPriority(pod)) + int64(math.MaxInt32+1)
			}
			// The smaller the sumPriorities, the higher the score.
			return -sumPriorities
		}
		minNumPodsScoreFunc := func(node string) int64 {
			// The smaller the length of pods, the higher the score.
			return -int64(len(nodesToVictims[node].Pods))
		}
		latestStartTimeScoreFunc := func(node string) int64 {
			// Get the earliest start time of all pods on the current node.
			earliestStartTimeOnNode := util.GetEarliestPodStartTime(nodesToVictims[node])
			if earliestStartTimeOnNode == nil {
				logger.Error(errors.New("earliestStartTime is nil for node"), "Should not reach here", "node", node)
				return int64(math.MinInt64)
			}
			// The bigger the earliestStartTimeOnNode, the higher the score.
			return earliestStartTimeOnNode.UnixNano()
		}

		// Each scoreFunc scores the nodes according to specific rules and keeps the name of the node
		// with the highest score. If and only if the scoreFunc has more than one node with the highest
		// score, we will execute the other scoreFunc in order of precedence.
		scoreFuncs = []func(string) int64{
			// A node with a minimum number of PDB is preferable.
			minNumPDBViolatingScoreFunc,
			// A node with a minimum highest priority victim is preferable.
			minHighestPriorityScoreFunc,
			// A node with the smallest sum of priorities is preferable.
			minSumPrioritiesScoreFunc,
			// A node with the minimum number of pods is preferable.
			minNumPodsScoreFunc,
			// A node with the latest start time of all highest priority victims is preferable.
			latestStartTimeScoreFunc,
			// If there are still ties, then the first Node in the list is selected.
		}
	}

	for _, f := range scoreFuncs {
		selectedNodes := []string{}
		maxScore := int64(math.MinInt64)
		for _, node := range allCandidates {
			score := f(node)
			if score > maxScore {
				maxScore = score
				selectedNodes = []string{}
			}
			if score == maxScore {
				selectedNodes = append(selectedNodes, node)
			}
		}
		if len(selectedNodes) == 1 {
			return selectedNodes[0]
		}
		allCandidates = selectedNodes
	}

	return allCandidates[0]
}

// getLowerPriorityNominatedPods returns pods whose priority is smaller than the
// priority of the given "pod" and are nominated to run on the given node.
// Note: We could possibly check if the nominated lower priority pods still fit
// and return those that no longer fit, but that would require lots of
// manipulation of NodeInfo and PreFilter state per nominated pod. It may not be
// worth the complexity, especially because we generally expect to have a very
// small number of nominated pods per node.
func getLowerPriorityNominatedPods(logger klog.Logger, pn framework.PodNominator, pod *v1.Pod, nodeName string) []*v1.Pod {
	podInfos := pn.NominatedPodsForNode(nodeName)

	if len(podInfos) == 0 {
		return nil
	}

	var lowerPriorityPods []*v1.Pod
	podPriority := corev1helpers.PodPriority(pod)
	for _, pi := range podInfos {
		if corev1helpers.PodPriority(pi.Pod) < podPriority {
			lowerPriorityPods = append(lowerPriorityPods, pi.Pod)
		}
	}
	return lowerPriorityPods
}

// DryRunPreemption simulates Preemption logic on <potentialNodes> in parallel,
// returns preemption candidates and a map indicating filtered nodes statuses.
// The number of candidates depends on the constraints defined in the plugin's args. In the returned list of
// candidates, ones that do not violate PDB are preferred over ones that do.
// NOTE: This method is exported for easier testing in default preemption.
func (ev *Evaluator) DryRunPreemption(ctx context.Context, pod *v1.Pod, potentialNodes []*framework.NodeInfo,
	pdbs []*policy.PodDisruptionBudget, offset int32, numCandidates int32) ([]Candidate, framework.NodeToStatusMap, error) {
	fh := ev.Handler
	nonViolatingCandidates := newCandidateList(numCandidates)
	violatingCandidates := newCandidateList(numCandidates)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	nodeStatuses := make(framework.NodeToStatusMap)
	var statusesLock sync.Mutex
	var errs []error
	checkNode := func(i int) {
		nodeInfoCopy := potentialNodes[(int(offset)+i)%len(potentialNodes)].Snapshot()
		stateCopy := ev.State.Clone()
		pods, numPDBViolations, status := ev.SelectVictimsOnNode(ctx, stateCopy, pod, nodeInfoCopy, pdbs)
		if status.IsSuccess() && len(pods) != 0 {
			victims := extenderv1.Victims{
				Pods:             pods,
				NumPDBViolations: int64(numPDBViolations),
			}
			c := &candidate{
				victims: &victims,
				name:    nodeInfoCopy.Node().Name,
			}
			if numPDBViolations == 0 {
				nonViolatingCandidates.add(c)
			} else {
				violatingCandidates.add(c)
			}
			nvcSize, vcSize := nonViolatingCandidates.size(), violatingCandidates.size()
			if nvcSize > 0 && nvcSize+vcSize >= numCandidates {
				cancel()
			}
			return
		}
		if status.IsSuccess() && len(pods) == 0 {
			status = framework.AsStatus(fmt.Errorf("expected at least one victim pod on node %q", nodeInfoCopy.Node().Name))
		}
		statusesLock.Lock()
		if status.Code() == framework.Error {
			errs = append(errs, status.AsError())
		}
		nodeStatuses[nodeInfoCopy.Node().Name] = status
		statusesLock.Unlock()
	}
	fh.Parallelizer().Until(ctx, len(potentialNodes), checkNode, ev.PluginName)
	return append(nonViolatingCandidates.get(), violatingCandidates.get()...), nodeStatuses, utilerrors.NewAggregate(errs)
}
