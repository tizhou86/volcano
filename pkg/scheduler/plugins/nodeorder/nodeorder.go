/*
Copyright 2019 The Kubernetes Authors.

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

package nodeorder

import (
	"fmt"

	"github.com/golang/glog"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/algorithm/priorities"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/api"
	"k8s.io/kubernetes/pkg/scheduler/cache"

	"github.com/kubernetes-sigs/kube-batch/pkg/scheduler/api"
	"github.com/kubernetes-sigs/kube-batch/pkg/scheduler/framework"
	"github.com/kubernetes-sigs/kube-batch/pkg/scheduler/plugins/util"
)

const (
	// NodeAffinityWeight is the key for providing Node Affinity Priority Weight in YAML
	NodeAffinityWeight = "nodeaffinity.weight"
	// PodAffinityWeight is the key for providing Pod Affinity Priority Weight in YAML
	PodAffinityWeight = "podaffinity.weight"
	// LeastRequestedWeight is the key for providing Least Requested Priority Weight in YAML
	LeastRequestedWeight = "leastrequested.weight"
	// BalancedResourceWeight is the key for providing Balanced Resource Priority Weight in YAML
	BalancedResourceWeight = "balancedresource.weight"
)

type nodeOrderPlugin struct {
	// Arguments given for the plugin
	pluginArguments framework.Arguments
}

func getInterPodAffinityScore(name string, interPodAffinityScore schedulerapi.HostPriorityList) int {
	for _, hostPriority := range interPodAffinityScore {
		if hostPriority.Host == name {
			return hostPriority.Score
		}
	}
	return 0
}

type cachedNodeInfo struct {
	session *framework.Session
}

func (c *cachedNodeInfo) GetNodeInfo(name string) (*v1.Node, error) {
	node, found := c.session.Nodes[name]
	if !found {
		for _, cacheNode := range c.session.Nodes {
			pods := cacheNode.Pods()
			for _, pod := range pods {
				if pod.Spec.NodeName == "" {
					return cacheNode.Node, nil
				}
			}
		}
		return nil, fmt.Errorf("failed to find node <%s>", name)
	}

	return node.Node, nil
}

//New function returns prioritizePlugin object
func New(aruguments framework.Arguments) framework.Plugin {
	return &nodeOrderPlugin{pluginArguments: aruguments}
}

func (pp *nodeOrderPlugin) Name() string {
	return "nodeorder"
}

type priorityWeight struct {
	leastReqWeight          int
	nodeAffinityWeight      int
	podAffinityWeight       int
	balancedRescourceWeight int
}

func calculateWeight(args framework.Arguments) priorityWeight {
	/*
	   User Should give priorityWeight in this format(nodeaffinity.weight, podaffinity.weight, leastrequested.weight, balancedresource.weight).
	   Currently supported only for nodeaffinity, podaffinity, leastrequested, balancedresouce priorities.

	   actions: "reclaim, allocate, backfill, preempt"
	   tiers:
	   - plugins:
	     - name: priority
	     - name: gang
	     - name: conformance
	   - plugins:
	     - name: drf
	     - name: predicates
	     - name: proportion
	     - name: nodeorder
	       arguments:
	         nodeaffinity.weight: 2
	         podaffinity.weight: 2
	         leastrequested.weight: 2
	         balancedresource.weight: 2
	*/

	// Values are initialized to 1.
	weight := priorityWeight{
		leastReqWeight:          1,
		nodeAffinityWeight:      1,
		podAffinityWeight:       1,
		balancedRescourceWeight: 1,
	}

	// Checks whether nodeaffinity.weight is provided or not, if given, modifies the value in weight struct.
	args.GetInt(&weight.nodeAffinityWeight, NodeAffinityWeight)

	// Checks whether podaffinity.weight is provided or not, if given, modifies the value in weight struct.
	args.GetInt(&weight.podAffinityWeight, PodAffinityWeight)

	// Checks whether leastrequested.weight is provided or not, if given, modifies the value in weight struct.
	args.GetInt(&weight.leastReqWeight, LeastRequestedWeight)

	// Checks whether balancedresource.weight is provided or not, if given, modifies the value in weight struct.
	args.GetInt(&weight.balancedRescourceWeight, BalancedResourceWeight)

	return weight
}

func (pp *nodeOrderPlugin) OnSessionOpen(ssn *framework.Session) {
	var nodeMap map[string]*cache.NodeInfo
	var nodeSlice []*v1.Node

	weight := calculateWeight(pp.pluginArguments)

	pl := util.NewPodLister(ssn)

	nl := &util.NodeLister{
		Session: ssn,
	}

	cn := &cachedNodeInfo{
		session: ssn,
	}

	nodeMap, nodeSlice = util.GenerateNodeMapAndSlice(ssn.Nodes)

	// Register event handlers to update task info in PodLister & nodeMap
	ssn.AddEventHandler(&framework.EventHandler{
		AllocateFunc: func(event *framework.Event) {
			pod := pl.UpdateTask(event.Task, event.Task.NodeName)

			nodeName := event.Task.NodeName
			node, found := nodeMap[nodeName]
			if !found {
				glog.Warningf("node order, update pod %s/%s allocate to NOT EXIST node [%s]", pod.Namespace, pod.Name, nodeName)
			} else {
				node.AddPod(pod)
				glog.V(4).Infof("node order, update pod %s/%s allocate to node [%s]", pod.Namespace, pod.Name, nodeName)
			}
		},
		DeallocateFunc: func(event *framework.Event) {
			pod := pl.UpdateTask(event.Task, "")

			nodeName := event.Task.NodeName
			node, found := nodeMap[nodeName]
			if !found {
				glog.Warningf("node order, update pod %s/%s allocate from NOT EXIST node [%s]", pod.Namespace, pod.Name, nodeName)
			} else {
				node.RemovePod(pod)
				glog.V(4).Infof("node order, update pod %s/%s deallocate from node [%s]", pod.Namespace, pod.Name, nodeName)
			}
		},
	})

	nodeOrderFn := func(task *api.TaskInfo, node *api.NodeInfo) (float64, error) {
		var interPodAffinityScore schedulerapi.HostPriorityList

		nodeInfo, found := nodeMap[node.Name]
		if !found {
			nodeInfo = cache.NewNodeInfo(node.Pods()...)
			nodeInfo.SetNode(node.Node)
			glog.Warningf("node order, generate node info for %s at NodeOrderFn is unexpected", node.Name)
		}
		var score = 0.0

		//TODO: Add ImageLocalityPriority Function once priorityMetadata is published
		//Issue: #74132 in kubernetes ( https://github.com/kubernetes/kubernetes/issues/74132 )

		host, err := priorities.LeastRequestedPriorityMap(task.Pod, nil, nodeInfo)
		if err != nil {
			glog.Warningf("Least Requested Priority Failed because of Error: %v", err)
			return 0, err
		}
		// If leastReqWeight in provided, host.Score is multiplied with weight, if not, host.Score is added to total score.
		score = score + float64(host.Score*weight.leastReqWeight)

		host, err = priorities.BalancedResourceAllocationMap(task.Pod, nil, nodeInfo)
		if err != nil {
			glog.Warningf("Balanced Resource Allocation Priority Failed because of Error: %v", err)
			return 0, err
		}
		// If balancedRescourceWeight in provided, host.Score is multiplied with weight, if not, host.Score is added to total score.
		score = score + float64(host.Score*weight.balancedRescourceWeight)

		host, err = priorities.CalculateNodeAffinityPriorityMap(task.Pod, nil, nodeInfo)
		if err != nil {
			glog.Warningf("Calculate Node Affinity Priority Failed because of Error: %v", err)
			return 0, err
		}
		// If nodeAffinityWeight in provided, host.Score is multiplied with weight, if not, host.Score is added to total score.
		score = score + float64(host.Score*weight.nodeAffinityWeight)

		mapFn := priorities.NewInterPodAffinityPriority(cn, nl, pl, v1.DefaultHardPodAffinitySymmetricWeight)
		interPodAffinityScore, err = mapFn(task.Pod, nodeMap, nodeSlice)
		if err != nil {
			glog.Warningf("Calculate Inter Pod Affinity Priority Failed because of Error: %v", err)
			return 0, err
		}
		hostScore := getInterPodAffinityScore(node.Name, interPodAffinityScore)
		// If podAffinityWeight in provided, host.Score is multiplied with weight, if not, host.Score is added to total score.
		score = score + float64(hostScore*weight.podAffinityWeight)

		glog.V(4).Infof("Total Score for that node is: %d", score)
		return score, nil
	}
	ssn.AddNodeOrderFn(pp.Name(), nodeOrderFn)
}

func (pp *nodeOrderPlugin) OnSessionClose(ssn *framework.Session) {
}
