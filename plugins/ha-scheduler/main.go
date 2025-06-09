package main

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1Listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

const (
	FAILURE_EVENT_NAME         = "FailedScheduling"
	SUCCESS_EVENT_NAME         = "Success"
	NODE                       = "Node"
	SchedulerName              = "ha-scheduler"
	HighAvailabilityLabel      = "isHighlyAvailable"
	HighAvailabilityAnnotation = "isHighlyAvailable"
)

type HAScheduler struct {
	clientset  kubernetes.Interface
	podQueue   chan *v1.Pod
	nodeLister corev1Listers.NodeLister
	podLister  corev1Listers.PodLister
}

func NewHAScheduler() (*HAScheduler, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := clientcmd.RecommendedHomeFile
		if kubeConfigEnvVar := clientcmd.RecommendedConfigPathEnvVar; kubeConfigEnvVar != "" {
			kubeconfig = kubeConfigEnvVar
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to build config: %v", err)
		}
	}

	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("Failed to get clientset: %v", err)
	}

	return &HAScheduler{
		clientset: clientSet,
		podQueue:  make(chan *v1.Pod, 300),
	}, nil
}

func (s *HAScheduler) Run(ctx context.Context) error {
	klog.InfoS("Starting Scheduler", "schedulerName", SchedulerName)

	informerFactory := informers.NewSharedInformerFactory(s.clientset, 30*time.Second)

	nodeInformer := informerFactory.Core().V1().Nodes()
	s.nodeLister = nodeInformer.Lister()

	podInformer := informerFactory.Core().V1().Pods()
	s.podLister = podInformer.Lister()

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    s.addPod,
		UpdateFunc: s.updatePod,
	})

	informerFactory.Start(ctx.Done())

	klog.InfoS("Caches synced, starting scheduling loop")

	go wait.UntilWithContext(ctx, s.scheduleOne, 0)

	<-ctx.Done()
	return nil
}

func (s *HAScheduler) addPod(obj interface{}) {
	pod := obj.(*v1.Pod)
	if s.shouldSchedule(pod) {
		klog.V(4).InfoS("Adding pod to queue", "pod", klog.KObj(pod))
		select {
		case s.podQueue <- pod:
		default:
			klog.ErrorS(nil, "pod queue is full. dropping pod", "pod", klog.KObj(pod))
		}
	}

}

func (s *HAScheduler) updatePod(oldObj, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	// Only Process pod if there is a change
	if oldPod.ResourceVersion == newPod.ResourceVersion {
		return
	}

	if s.shouldSchedule(newPod) {
		klog.V(4).InfoS("Updating pod in queue", "pod", klog.KObj(newPod))
		select {
		case s.podQueue <- newPod:
		default:
			klog.ErrorS(nil, "pod queue is full. dropping pod", "pod", klog.KObj(newPod))
		}
	}
}

func (s *HAScheduler) shouldSchedule(pod *v1.Pod) bool {
	// Only handle pods assigned to our scheduler
	if pod.Spec.SchedulerName != SchedulerName {
		return false
	}

	// Only handle unscheduled pods
	if pod.Spec.NodeName != "" {
		return false
	}

	// Skip if pod is being deleted
	if pod.DeletionTimestamp != nil {
		return false
	}

	// Skip if pod is in terminal state
	if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
		return false
	}

	return true
}

func (s *HAScheduler) scheduleOne(ctx context.Context) {
	select {
	case pod := <-s.podQueue:
		err := s.schedulePod(ctx, pod)
		if err != nil {
			klog.ErrorS(err, "Failed to schedule pod", "pod", klog.KObj(pod))
			go func() {
				time.Sleep(5 * time.Second)
				select {
				case s.podQueue <- pod:
				default:
					klog.ErrorS(nil, "Failed to requeue pod for scheduling", "pod", klog.KObj(pod))
				}
			}()
		}
	case <-ctx.Done():
		return
	}
}

// schedulerPod Schedule a pod on the node
func (s *HAScheduler) schedulePod(ctx context.Context, pod *v1.Pod) error {
	klog.InfoS("Attempting to schedule pod", "pod", klog.KObj(pod))

	nodes, err := s.nodeLister.List(labels.Everything())

	if err != nil {
		return fmt.Errorf("failed to get nodes %v", err)
	}

	if len(nodes) == 0 {
		err := s.RecordSchedulingFailure(ctx, pod, "No nodes available in the cluster")
		if err != nil {
			klog.Errorf("failed to create scheduling event %s", err)
		}
		return fmt.Errorf("no suitable nodes found for pod %s", pod.Name)
	}

	klog.InfoS("Getting suitable nodes")
	suitableNodes := s.filterNodes(ctx, pod, nodes)

	if len(suitableNodes) == 0 {
		s.RecordSchedulingFailure(ctx, pod, "No nodes match HA requirements or other constraints")
		return fmt.Errorf("no suitable nodes found for pod %s", pod.Name)
	}

	selectedNode := s.selectBestNode(suitableNodes)

	klog.InfoS("Selected Pod", "pod", klog.KObj(selectedNode))

	err = s.bindPodToNode(ctx, pod, selectedNode)

	if err != nil {
		return fmt.Errorf("failed to bind pod to node: %v", err)
	}

	klog.InfoS("Successfully scheduled pod",
		"pod", klog.KObj(pod),
		"node", selectedNode.Name)

	return nil
}

func (s *HAScheduler) selectBestNode(nodes []*v1.Node) *v1.Node {
	// Simplified node selection - just return the first node
	// In production, you'd implement scoring algorithms like:
	// - Least allocated resources
	// - Node affinity preferences
	// - Balanced resource usage
	// - Custom scoring functions

	if len(nodes) == 0 {
		return nil
	}

	// For now, just return the first suitable node
	return nodes[0]
}

func (s *HAScheduler) bindPodToNode(ctx context.Context, pod *v1.Pod, node *v1.Node) error {
	klog.V(3).InfoS("Binding pod to node", "pod", klog.KObj(pod), "node", klog.KObj(node))

	binding := &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		Target: v1.ObjectReference{
			Kind: NODE,
			Name: node.Name,
		},
	}

	err := s.clientset.CoreV1().Pods(pod.Namespace).Bind(ctx, binding, metav1.CreateOptions{})

	if err != nil {
		return err
	}

	err = s.RecordSchedulingSuccess(ctx, pod, node)
	if err != nil {
		klog.Errorf("Failed to create scheduling event %s", err)
		return err
	}

	return nil
}

func (s *HAScheduler) filterNodes(ctx context.Context, pod *v1.Pod, nodes []*v1.Node) []*v1.Node {
	var suitableNodes []*v1.Node

	podRequiresHA := false

	if pod.Labels != nil {
		if val, exists := pod.Labels[HighAvailabilityLabel]; exists && val == "true" {
			podRequiresHA = true
		}
	}

	klog.V(4).InfoS("Filtering nodes",
		"pod", klog.KObj(pod),
		"totalNodes", len(nodes),
		"podRequiresHA", podRequiresHA)

	for _, node := range nodes {

		if !s.isNodeReady(node) {
			klog.InfoS("Skipping node - node ready", "node", klog.KObj(node))
		}

		if node.DeletionTimestamp != nil {
			klog.V(5).InfoS("Skipping node - being deleted", "node", klog.KObj(node))
		}

		// Check if node is schedulable
		if node.Spec.Unschedulable {
			klog.V(5).InfoS("Skipping node - marked unschedulable", "node", node.Name)
			continue
		}

		// Check taints (simplified - in production you'd check tolerations)
		if s.hasBlockingTaints(node, pod) {
			klog.V(5).InfoS("Skipping node - has blocking taints", "node", node.Name)
			continue
		}

		if podRequiresHA {
			nodeSupportsHA := false
			if node.Annotations != nil {
				if val, exists := node.Annotations[HighAvailabilityAnnotation]; exists && val == "true" {
					nodeSupportsHA = true
				}
			}

			if !nodeSupportsHA {
				klog.V(4).InfoS("Skipping node - does not support HA", "node", klog.KObj(node), "pod", klog.KObj(pod))
				continue
			}
		}

		klog.V(4).InfoS("Node passed all filters", "node", node.Name)
		suitableNodes = append(suitableNodes, node)
	}

	klog.V(3).InfoS("Node filtering complete",
		"pod", klog.KObj(pod),
		"suitableNodes", len(suitableNodes),
		"totalNodes", len(nodes))

	return suitableNodes
}

func (s *HAScheduler) hasEnoughResources(ctx context.Context, pod *v1.Pod, node *v1.Node) bool {
	pods, err := s.podLister.List(labels.Everything())

	if err != nil {
		klog.ErrorS(err, "Failed to list pods for resource check", "node", node.Name)
		return false
	}

	var podsOnNode []*v1.Pod
	for _, p := range pods {
		if p.Spec.NodeName == node.Name && p.Status.Phase != v1.PodSucceeded && p.Status.Phase != v1.PodFailed {
			podsOnNode = append(podsOnNode, p)
		}
	}

	maxPodsPerNode := 110 // Default Kubernetes limit
	if len(podsOnNode) >= maxPodsPerNode {
		return false
	}

	return true
}

func (s *HAScheduler) isNodeReady(node *v1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == v1.NodeReady {
			return condition.Status == v1.ConditionTrue
		}
	}
	return false
}

func (s *HAScheduler) hasBlockingTaints(node *v1.Node, pod *v1.Pod) bool {
	// Simplified taint checking - in production, check pod tolerations
	for _, taint := range node.Spec.Taints {
		if taint.Effect == v1.TaintEffectNoSchedule || taint.Effect == v1.TaintEffectNoExecute {
			// For simplicity, we'll skip nodes with these taints
			// In production, you'd check if the pod tolerates these taints
			return true
		}
	}
	return false
}

func (s *HAScheduler) RecordSchedulingFailure(ctx context.Context, pod *v1.Pod, reason string) error {
	message := fmt.Sprintf("Failed to schedule pod %s", pod.Name)
	return s.recordEvent(ctx, pod, FAILURE_EVENT_NAME, reason, message)
}

func (s *HAScheduler) RecordSchedulingSuccess(ctx context.Context, pod *v1.Pod, node *v1.Node) error {
	message := fmt.Sprintf("Successfully scheduled pod %s to node %s", pod.Name, node.Name)
	return s.recordEvent(ctx, pod, SUCCESS_EVENT_NAME, "", message)
}

func (s *HAScheduler) recordEvent(ctx context.Context, pod *v1.Pod, eventType, reason, message string) error {
	event := &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", pod.Name),
			Namespace:    pod.Namespace,
		},
		InvolvedObject: v1.ObjectReference{
			Kind:       "pod",
			Name:       pod.Name,
			Namespace:  pod.Namespace,
			UID:        pod.UID,
			APIVersion: pod.APIVersion,
		},
		Reason:  reason,
		Message: message,
		Type:    eventType,
		Source: v1.EventSource{
			Component: pod.Spec.SchedulerName,
		},
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
		Count:          1,
	}

	_, err := s.clientset.CoreV1().Events(pod.Namespace).Create(ctx, event, metav1.CreateOptions{})

	if err != nil {
		klog.ErrorS(err, "Failed to record event", "pod", klog.KObj(pod))
	}
	return nil
}

func main() {
	klog.InitFlags(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheduler, err := NewHAScheduler()
	if err != nil {
		klog.Fatal("Failed to create scheduler: ", err)
	}

	klog.InfoS("Starting HA Scheduler main loop")
	if err := scheduler.Run(ctx); err != nil {
		klog.Fatal("Scheduler failed: ", err)
	}
}
