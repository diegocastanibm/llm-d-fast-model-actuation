/*
Copyright 2025 The llm-d Authors.

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

package dualpods

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/spf13/pflag"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1preinformers "k8s.io/client-go/informers/core/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/llm-d-incubation/llm-d-fast-model-actuation/pkg/api"
	genctlr "github.com/llm-d-incubation/llm-d-fast-model-actuation/pkg/controller/generic"
)

const ControllerName = "dual-pods-controller"
const GPUMapName = "gpu-map"

type Controller interface {
	Start(context.Context) error
}

type CommonConfig struct {
	Verbosity int // `-v`
}

func (cc *CommonConfig) AddToFlagSet(name string, flags *pflag.FlagSet) {
	flags.IntVar(&cc.Verbosity, name+"-verbosity", cc.Verbosity, "-v setting for "+name)
}

// NewController makes a new dual pods controller.
func NewController(
	logger klog.Logger,
	coreClient coreclient.CoreV1Interface,
	corev1PreInformers corev1preinformers.Interface,
	numWorkers int,
) (*controller, error) {
	ctl := &controller{
		enqueueLogger: logger.WithName(ControllerName),
		coreclient:    coreClient,
		podInformer:   corev1PreInformers.Pods().Informer(),
		podLister:     corev1PreInformers.Pods().Lister(),
		cmInformer:    corev1PreInformers.ConfigMaps().Informer(),
		cmLister:      corev1PreInformers.ConfigMaps().Lister(),
		nodeInformer:  corev1PreInformers.Nodes().Informer(),
		nodeLister:    corev1PreInformers.Nodes().Lister(),
	}
	ctl.gpuMap.Store(&map[string]GpuLocation{})
	ctl.QueueAndWorkers = genctlr.NewQueueAndWorkers(string(ControllerName), numWorkers, ctl.process)
	_, err := ctl.podInformer.AddEventHandler(ctl)
	if err != nil {
		panic(err)
	}
	_, err = ctl.cmInformer.AddEventHandler(ctl)
	if err != nil {
		panic(err)
	}
	return ctl, nil
}

type controller struct {
	enqueueLogger klog.Logger
	coreclient    coreclient.CoreV1Interface
	podInformer   cache.SharedIndexInformer
	podLister     corev1listers.PodLister
	cmInformer    cache.SharedIndexInformer
	cmLister      corev1listers.ConfigMapLister
	nodeInformer  cache.SharedIndexInformer
	nodeLister    corev1listers.NodeLister
	genctlr.QueueAndWorkers[typedRef]

	// gpuMaps maps GPU UUID to GpuLocation
	gpuMap atomic.Pointer[map[string]GpuLocation]
}

type GpuLocation struct {
	Node  string
	Index uint
}

var _ Controller = &controller{}

type typedRef struct {
	Kind string
	cache.ObjectName
}

func (ref typedRef) String() string {
	return ref.Kind + ":" + ref.ObjectName.String()
}

const podKind = "Pod"
const cmKind = "ConfigMap"

func (ctl *controller) careAbout(pod *corev1.Pod) bool {
	role := pod.Annotations[api.PodRoleAnnotationName]
	if role != api.PodRoleAnnotationValueRequesting && role != api.PodRoleAnnotationValueRunning {
		ctl.enqueueLogger.V(5).Info("Pod has no role annotation or unknown role so don't care", "name", pod.Name, "role", role)
		return false
	}
	return true
}

func (ctl *controller) OnAdd(obj any, isInInitialList bool) {
	var kind string
	var objM metav1.Object
	switch typed := obj.(type) {
	case *corev1.Pod:
		if !ctl.careAbout(typed) {
			return
		}
		objM = typed
		kind = podKind
	case *corev1.ConfigMap:
		if typed.Name != GPUMapName {
			return
		}
		objM = typed
		kind = cmKind
	default:
		ctl.enqueueLogger.Error(nil, "Notified of add of unexpected type of object", "type", fmt.Sprintf("%T", obj))
		return
	}
	ref := typedRef{kind, cache.MetaObjectToName(objM)}
	ctl.enqueueLogger.V(5).Info("Enqueuing reference due to notification of add", "ref", ref, "isInInitialList", isInInitialList)
	ctl.Queue.Add(ref)

}

func (ctl *controller) OnUpdate(prev, obj any) {
	var kind string
	var objM metav1.Object
	switch typed := obj.(type) {
	case *corev1.Pod:
		if !ctl.careAbout(typed) {
			return
		}
		objM = typed
		kind = podKind
	case *corev1.ConfigMap:
		if typed.Name != GPUMapName {
			return
		}
		objM = typed
		kind = cmKind
	default:
		ctl.enqueueLogger.Error(nil, "Notified of update of unexpected type of object", "type", fmt.Sprintf("%T", obj))
		return
	}
	ref := typedRef{kind, cache.MetaObjectToName(objM)}
	ctl.enqueueLogger.V(5).Info("Enqueuing reference due to notification of update", "ref", ref)
	ctl.Queue.Add(ref)
}

func (ctl *controller) OnDelete(obj any) {
	if dfsu, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = dfsu.Obj
	}
	var kind string
	var objM metav1.Object
	switch typed := obj.(type) {
	case *corev1.Pod:
		if !ctl.careAbout(typed) {
			return
		}
		objM = typed
		kind = podKind
	case *corev1.ConfigMap:
		if typed.Name != GPUMapName {
			return
		}
		objM = typed
		kind = cmKind
	default:
		ctl.enqueueLogger.Error(nil, "Notified of delete of unexpected type of object", "type", fmt.Sprintf("%T", obj))
		return
	}
	ref := typedRef{kind, cache.MetaObjectToName(objM)}
	ctl.enqueueLogger.V(5).Info("Enqueuing reference due to notification of delete", "ref", ref)
	ctl.Queue.Add(ref)
}

func (ctl *controller) Start(ctx context.Context) error {
	if !cache.WaitForNamedCacheSync(ControllerName, ctx.Done(), ctl.cmInformer.HasSynced, ctl.podInformer.HasSynced, ctl.nodeInformer.HasSynced) {
		return fmt.Errorf("caches not synced before end of Start context")
	}
	err := ctl.StartWorkers(ctx)
	if err != nil {
		return fmt.Errorf("failed to start workers: %w", err)
	}
	return nil
}

// process returns (err error, retry bool).
// There will be a retry iff `retry || err != nil`.
func (ctl *controller) process(ctx context.Context, ref typedRef) (error, bool) {
	logger := klog.FromContext(ctx)
	switch ref.Kind {
	case podKind:
		return ctl.processPod(ctx, ref.ObjectName)
	case cmKind:
		return ctl.processConfigMap(ctx, ref.ObjectName)
	default:
		logger.Error(nil, "Asked to process unexpected Kind of object", "kind", ref.Kind)
		return nil, false
	}
}

func (ctl *controller) processPod(ctx context.Context, podRef cache.ObjectName) (error, bool) {
	logger := klog.FromContext(ctx)
	logger.V(5).Info("Processing Pod", "name", podRef.Name)

	got, err := ctl.podLister.Pods(podRef.Namespace).Get(podRef.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.V(5).Info("Pod not found, skipping processing", "name", podRef.Name)
			return nil, false
		}
		logger.Error(err, "Failed to get Pod", "name", podRef.Name)
		return err, true
	}

	role := got.Annotations[api.PodRoleAnnotationName]
	switch role {
	case api.PodRoleAnnotationValueRequesting:
		return ctl.processServerRequestingPod(ctx, got)
	case api.PodRoleAnnotationValueRunning:
		return ctl.processServerRunningPod(ctx, got)
	default:
		logger.V(5).Info("Pod has no role annotation or unknown role, skipping processing", "name", podRef.Name, "role", role)
		return nil, false
	}
}

func (ctl *controller) processConfigMap(ctx context.Context, cmRef cache.ObjectName) (error, bool) {
	logger := klog.FromContext(ctx)
	cm, err := ctl.coreclient.ConfigMaps(cmRef.Namespace).Get(ctx, cmRef.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			ctl.gpuMap.Store(nil)
			logger.V(1).Info("ConfigMap " + GPUMapName + " does not exist")
		}
		return err, true
	}
	newMap := map[string]GpuLocation{}
	nodeCount := 0
	for nodeName, mapStr := range cm.Data {
		_, err := ctl.nodeLister.Get(nodeName)
		if err != nil && errors.IsNotFound(err) {
			logger.V(3).Info("Ignoring entry in GPU map because key is not a Node name", "nodeName", nodeName)
			continue
		}
		var nodesMap map[string]uint
		err = json.Unmarshal([]byte(mapStr), &nodesMap)
		if err != nil {
			logger.Error(err, "A GPU map entry failed to parse as JSON", "nodeName", nodeName)
			continue
		}
		for uuid, index := range nodesMap {
			newMap[uuid] = GpuLocation{Node: nodeName, Index: index}
		}
		nodeCount += 1
	}
	logger.V(1).Info("Parsed GPU map", "numNodes", nodeCount, "numGPUs", len(newMap))
	ctl.gpuMap.Store(&newMap)
	return nil, false
}
