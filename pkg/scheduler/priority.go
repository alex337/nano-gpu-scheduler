package scheduler

import (
	"context"

	"github.com/nano-gpu/nano-gpu-scheduler/pkg/dealer"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	log "k8s.io/klog/v2"
	extender "k8s.io/kube-scheduler/extender/v1"
)

type Prioritize struct {
	Name string
	Func func(pod *v1.Pod, nodeNames []string) (*extender.HostPriorityList, error)
}

func (p Prioritize) Handler(args extender.ExtenderArgs) (*extender.HostPriorityList, error) {
	pod := args.Pod
	nodeNames := *args.NodeNames
	return p.Func(pod, nodeNames)
}

func NewNanoGPUPrioritize(ctx context.Context, clientset *kubernetes.Clientset, d dealer.Dealer, policySpec dealer.PolicySpec, isLoadSchedule bool) *Prioritize {
	return &Prioritize{
		Name: "NanoGPUSorter",
		Func: func(pod *v1.Pod, nodeNames []string) (*extender.HostPriorityList, error) {
			var priorityList extender.HostPriorityList
			priorityList = make([]extender.HostPriority, len(nodeNames))
			scores := d.Score(nodeNames, pod, policySpec, isLoadSchedule)
			for i, score := range scores {
				priorityList[i] = extender.HostPriority{
					Host:  nodeNames[i],
					Score: int64(score),
				}
			}
			log.Infof("node scores: %v", priorityList)
			return &priorityList, nil
		},
	}
}
