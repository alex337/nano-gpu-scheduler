package dealer

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"sync"
	"time"

	schetypes "github.com/nano-gpu/nano-gpu-scheduler/pkg/types"
	"github.com/nano-gpu/nano-gpu-scheduler/pkg/utils"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	log "k8s.io/klog/v2"
)

const OptimisticLockErrorMsg = "the object has been modified; please apply your changes to the latest version and try again"

type Dealer interface {
	Assume(nodes []string, pod *v1.Pod, policySpec PolicySpec, isLoadSchedule bool) ([]bool, []error)
	Score(node []string, pod *v1.Pod, policySpec PolicySpec, isLoadSchedule bool) []int
	Bind(node string, pod *v1.Pod, policySpec PolicySpec, isLoadSchedule bool) error
	Allocate(pod *v1.Pod) error
	Release(pod *v1.Pod) error
	Forget(pod *v1.Pod) error
	KnownPod(pod *v1.Pod) bool
	PodReleased(pod *v1.Pod) bool
	PrintStatus(pod *v1.Pod, action string)
	Status() (map[string]*NodeInfo, error)
	GetCoreUsage(nodeName string) (map[int]GPUCoreUsage, bool)
	GetMemoryUsage(nodeName string) (map[int]GPUMemoryUsage, bool)
	GetMemoryUsageLock(nodeName string) (map[int]GPUMemoryUsage, bool)
	GetCoreUsageLock(nodeName string) (map[int]GPUCoreUsage, bool)
	AddCoreUsage(nodeName string)
	AddMemoryUsage(nodeName string)
	UpdateCoreUsage(nodeName, coreUsage, updateTime string, cardNum int)
	UpdateMemoryUsage(nodeName, memoryUsage, updateTime string, cardNum int)
	GetUsage(nodeName, key string, card int, activeDuration time.Duration) (bool, float64, error)
}

func NewDealer(clientset *kubernetes.Clientset, nodeLister corelisters.NodeLister, podLister corelisters.PodLister, rater Rater) (Dealer, error) {
	di := &DealerImpl{
		Client:         clientset,
		NodeLister:     nodeLister,
		PodLister:      podLister,
		Rater:          rater,
		Lock:           sync.Mutex{},
		PodMaps:        make(map[types.UID]*v1.Pod),
		NodeMaps:       make(map[string]*NodeInfo),
		CoreUsage:      make(map[string]map[int]GPUCoreUsage),
		MemoryUsage:    make(map[string]map[int]GPUMemoryUsage),
		ReleasedPodMap: make(map[types.UID]struct{}),
	}
	pods, err := clientset.CoreV1().Pods(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", schetypes.GPUAssume, "true"),
	})
	if err != nil {
		return nil, err
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == "" {
			continue
		}
		if _, err := di.getNodeInfo(pod.Spec.NodeName); err != nil {
			log.Errorf("get node %s failed: %s", pod.Spec.NodeName, err.Error())
			continue
		}
	}
	return di, nil
}

type DealerImpl struct {
	Client     *kubernetes.Clientset
	NodeLister corelisters.NodeLister
	PodLister  corelisters.PodLister
	Rater          Rater
	Lock           sync.Mutex
	PodMaps        map[types.UID]*v1.Pod
	NodeMaps       map[string]*NodeInfo
	CoreUsage      map[string]map[int]GPUCoreUsage
	MemoryUsage    map[string]map[int]GPUMemoryUsage
	ReleasedPodMap map[types.UID]struct{}
}

func (d *DealerImpl) Assume(nodes []string, pod *v1.Pod, policySpec PolicySpec, isLoadSchedule bool) ([]bool, []error) {
	d.Lock.Lock()
	defer d.Lock.Unlock()

	demand := NewDemandFromPod(pod)
	res := make([]error, len(nodes))
	ans := make([]bool, len(nodes))
	nodeInfos := make([]*NodeInfo, len(nodes))
	for i, name := range nodes {
		ni, err := d.getNodeInfo(name)
		if err != nil {
			ni = nil
			ans[i] = false
			res[i] = fmt.Errorf("nano gpu scheduler get node failed: %v", err)
		}
		nodeInfos[i] = ni
	}

	ch := make(chan int, len(nodeInfos))
	wg := sync.WaitGroup{}
	for i := 0; i < len(nodeInfos); i++ {
		ch <- i
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case number := <-ch:
					if nodeInfos[number] == nil {
						continue
					}
					nodeInfos[number].cleanPlan()
					assumed, err := nodeInfos[number].Assume(demand, d, policySpec, isLoadSchedule)
					ans[number] = assumed
					res[number] = err
				default:
					return
				}
			}

		}()
	}
	wg.Wait()
	return ans, res
}

func (d *DealerImpl) Score(nodes []string, pod *v1.Pod, policySpec PolicySpec, isLoadSchedule bool) []int {
	d.Lock.Lock()
	defer d.Lock.Unlock()
	demand := NewDemandFromPod(pod)
	scores := make([]int, len(nodes))
	for i := 0; i < len(nodes); i++ {
		ni, err := d.getNodeInfo(nodes[i])
		if err != nil {
			log.Errorf("score pod %s/%s not found target node %s: %s", pod.Namespace, pod.Name, nodes[i], err.Error())
			scores[i] = ScoreMin
			continue
		}
		scores[i] = ni.Score(demand, d, policySpec, isLoadSchedule)
	}
	return scores
}

func (d *DealerImpl) Bind(node string, pod *v1.Pod, policySpec PolicySpec, isLoadSchedule bool) (err error) {
	d.Lock.Lock()
	defer d.Lock.Unlock()

	ni, err := d.getNodeInfo(node)
	if err != nil {
		return err
	}
	nodeInfo, err := d.NodeLister.Get(ni.Name)
	if err != nil {
		return err
	}
	anno := nodeInfo.ObjectMeta.Annotations
	if anno == nil {
		anno = map[string]string{}
	}
	plan, err := ni.Bind(NewDemandFromPod(pod), d, policySpec, isLoadSchedule)
	if err != nil {
		return err
	}

	newPod := utils.GetUpdatedPodAnnotationSpec(pod, plan.GPUIndexes)
	if _, err := d.Client.CoreV1().Pods(newPod.Namespace).Update(context.Background(), newPod, metav1.UpdateOptions{}); err != nil {
		if err.Error() == OptimisticLockErrorMsg {
			pod, err = d.Client.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			newPod = utils.GetUpdatedPodAnnotationSpec(pod, plan.GPUIndexes)
			if _, err = d.Client.CoreV1().Pods(pod.Namespace).Update(context.Background(), newPod, metav1.UpdateOptions{}); err != nil {
				return err
			}
		} else {
			return nil
		}
	}
	if err := d.Client.CoreV1().Pods(newPod.Namespace).Bind(context.Background(), &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{Namespace: newPod.Namespace, Name: newPod.Name, UID: newPod.UID},
		Target: v1.ObjectReference{
			Kind: "Node",
			Name: node,
		},
	}, metav1.CreateOptions{}); err != nil {
		return err
	}
	d.PodMaps[pod.UID] = newPod

	return nil
}

func (d *DealerImpl) Allocate(pod *v1.Pod) error {
	d.Lock.Lock()
	defer d.Lock.Unlock()
	if pod.Spec.NodeName == "" {
		return fmt.Errorf("pod %s/%s nodename is empty", pod.Namespace, pod.Name)
	}
	ni, err := d.getNodeInfo(pod.Spec.NodeName)
	if err != nil {
		return err
	}
	if _, ok := d.PodMaps[pod.UID]; ok {
		return nil
	}
	plan, err := NewPlanFromPod(pod)
	if err != nil {
		return err
	}
	err = ni.Allocate(plan)
	if err != nil {
		return err
	}
	d.PodMaps[pod.UID] = pod
	return nil
}

func (d *DealerImpl) Release(pod *v1.Pod) error {
	d.Lock.Lock()
	defer d.Lock.Unlock()

	ni, err := d.getNodeInfo(pod.Spec.NodeName)
	if err != nil {
		log.Errorf("release pod %s failed: %s", pod.Name, err.Error())
		return err
	}
	if _, ok := d.PodMaps[pod.UID]; !ok {
		log.Errorf("no such pod %s/%s", pod.Namespace, pod.Name)
		return nil
	}
	plan, err := NewPlanFromPod(pod)
	if err != nil {
		log.Errorf("create plan from pod failed: %s", err.Error())
		return err
	}
	if err := ni.Release(plan); err != nil {
		log.Errorf("release pod %s failed: node info release failed: %s", pod.Name, err.Error())
		return err
	}
	delete(d.PodMaps, pod.UID)
	d.ReleasedPodMap[pod.UID] = struct{}{}
	return nil
}

func (d *DealerImpl) KnownPod(pod *v1.Pod) bool {
	d.Lock.Lock()
	defer d.Lock.Unlock()
	_, ok := d.PodMaps[pod.UID]
	return ok
}

func (d *DealerImpl) PodReleased(pod *v1.Pod) bool {
	d.Lock.Lock()
	defer d.Lock.Unlock()
	_, ok := d.ReleasedPodMap[pod.UID]
	return ok
}

func (d *DealerImpl) getNodeInfo(name string) (*NodeInfo, error) {
	if ni, ok := d.NodeMaps[name]; ok {
		return ni, nil
	}
	node, err := d.NodeLister.Get(name)
	if err != nil {
		return nil, err
	}
	pods, err := d.Client.CoreV1().Pods(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", schetypes.GPUAssume, "true"),
		FieldSelector: fields.OneTermEqualSelector(schetypes.NodeNameField, name).String(),
	})
	if err != nil {
		return nil, err
	}
	d.NodeMaps[name] = NewNodeInfo(name, node, d.Rater)
	for _, pod := range pods.Items {
		// todo: check pod status
		plan, err := NewPlanFromPod(&pod)
		if err != nil {
			log.Errorf("stat pod %s/%s failed: %s", pod.Namespace, pod.Name, err.Error())
			continue
		}
		if err := d.NodeMaps[name].Allocate(plan); err != nil {
			log.Errorf("allocate pod %s/%s failed: %s", pod.Namespace, pod.Name, err.Error())
			continue
		}
		d.PodMaps[pod.UID] = &pod
	}
	return d.NodeMaps[name], nil
}

func (d *DealerImpl) PrintStatus(pod *v1.Pod, action string) {
	log.Infof("------resource status after %s for %s/%s------", action, pod.Namespace, pod.Name)
	for name, node := range d.NodeMaps {
		log.Infof("node %s: %v\n", name, node.GPUs)
	}
	log.Infof("------------")
}

func (d *DealerImpl) Forget(pod *v1.Pod) error {
	d.Lock.Lock()
	defer d.Lock.Unlock()

	delete(d.ReleasedPodMap, pod.UID)
	delete(d.PodMaps, pod.UID)

	return nil
}

func (d *DealerImpl) Status() (map[string]*NodeInfo, error) {
	return d.NodeMaps, nil
}
