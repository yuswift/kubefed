/*
Copyright 2016 The Kubernetes Authors.

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

package kubefedcluster
// kubefedcluster controller 是手写的 不是用kubebuilder生成的
// 功能比较简单 只是把一些client 放进内存 缓存起来 不会做其他多余的事情
// 这里没有finalizer这个字段 我猜测是因为这个controller的功能比较简单 没有其他关联资源

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	kubeclient "k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"

	fedv1b1 "sigs.k8s.io/kubefed/pkg/apis/core/v1beta1"
	genericclient "sigs.k8s.io/kubefed/pkg/client/generic"
	genscheme "sigs.k8s.io/kubefed/pkg/client/generic/scheme"
	"sigs.k8s.io/kubefed/pkg/controller/util"
	"sigs.k8s.io/kubefed/pkg/features"
	"sigs.k8s.io/kubefed/pkg/metrics"
)

// ClusterData stores cluster client and previous health check probe results of individual cluster.
// 每个集群的名字都会对应一个clusterdata 方便其他的controller使用
type ClusterData struct {
	// clusterKubeClient is the kube client for the cluster.
	clusterKubeClient *ClusterClient

	// clusterStatus is the cluster status as of last sampling.
	clusterStatus *fedv1b1.KubeFedClusterStatus

	// How many times in a row the probe has returned the same result.
	resultRun int64

	// cachedObj holds the last observer object from apiserver
	cachedObj *fedv1b1.KubeFedCluster
}

// ClusterController is responsible for maintaining the health status of each
// KubeFedCluster in a particular namespace.
type ClusterController struct {
	client genericclient.Client

	// clusterHealthCheckConfig is the configurable parameters for cluster health check
	clusterHealthCheckConfig *util.ClusterHealthCheckConfig

	// 为了线程安全加的一把锁 因为用的是原生的map
	mu sync.RWMutex

	// clusterDataMap is a mapping of clusterName and the cluster specific details.
	clusterDataMap map[string]*ClusterData

	// clusterController is the cache.Controller where callbacks are registered
	// for events on KubeFedClusters.
	clusterController cache.Controller

	// fedNamespace is the name of the namespace containing
	// KubeFedCluster resources and their associated secrets.
	fedNamespace string

	eventRecorder record.EventRecorder
}

// StartClusterController starts a new cluster controller.
func StartClusterController(config *util.ControllerConfig, clusterHealthCheckConfig *util.ClusterHealthCheckConfig, stopChan <-chan struct{}) error {
	controller, err := newClusterController(config, clusterHealthCheckConfig)
	if err != nil {
		return err
	}
	klog.Infof("Starting cluster controller")
	controller.Run(stopChan)
	return nil
}

// newClusterController returns a new cluster controller
// 初始化controller的方式比较native 增加几个回调函数 跟sample-controller的代码非常类似
// crud本质上都是对map的操作
func newClusterController(config *util.ControllerConfig, clusterHealthCheckConfig *util.ClusterHealthCheckConfig) (*ClusterController, error) {
	kubeConfig := restclient.CopyConfig(config.KubeConfig)
	kubeConfig.Timeout = clusterHealthCheckConfig.Timeout
	client := genericclient.NewForConfigOrDieWithUserAgent(kubeConfig, "cluster-controller")

	cc := &ClusterController{
		client:                   client,
		clusterHealthCheckConfig: clusterHealthCheckConfig,
		clusterDataMap:           make(map[string]*ClusterData),
		fedNamespace:             config.KubeFedNamespace,
	}

	kubeClient := kubeclient.NewForConfigOrDie(kubeConfig)
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(genscheme.Scheme, corev1.EventSource{Component: "kubefedcluster-controller"})
	cc.eventRecorder = recorder

	var err error
	_, cc.clusterController, err = util.NewGenericInformerWithEventHandler(
		config.KubeConfig,
		config.KubeFedNamespace,
		&fedv1b1.KubeFedCluster{},
		util.NoResyncPeriod,
		&cache.ResourceEventHandlerFuncs{
			DeleteFunc: func(obj interface{}) {
				castObj, ok := obj.(*fedv1b1.KubeFedCluster)
				if !ok {
					tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
					if !ok {
						klog.Errorf("Couldn't get object from tombstone %#v", obj)
						return
					}
					castObj, ok = tombstone.Obj.(*fedv1b1.KubeFedCluster)
					if !ok {
						klog.Errorf("Tombstone contained object that is not expected %#v", obj)
						return
					}
				}
				cc.delFromClusterSet(castObj)
			},
			AddFunc: func(obj interface{}) {
				castObj := obj.(*fedv1b1.KubeFedCluster)
				cc.addToClusterSet(castObj)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				var clusterChanged bool
				cluster := newObj.(*fedv1b1.KubeFedCluster)
				cc.mu.Lock()
				clusterData, ok := cc.clusterDataMap[cluster.Name]

				if !ok || !equality.Semantic.DeepEqual(clusterData.cachedObj.Spec, cluster.Spec) ||
					!equality.Semantic.DeepEqual(clusterData.cachedObj.ObjectMeta.Annotations, cluster.ObjectMeta.Annotations) ||
					!equality.Semantic.DeepEqual(clusterData.cachedObj.ObjectMeta.Labels, cluster.ObjectMeta.Labels) {
					clusterChanged = true
				}
				cc.mu.Unlock()
				// ignore update if there is no change between the cached object and new
				if !clusterChanged {
					return
				}
				cc.delFromClusterSet(cluster)
				cc.addToClusterSet(cluster)
			},
		},
	)
	return cc, err
}

// delFromClusterSet removes a cluster from the cluster data map
func (cc *ClusterController) delFromClusterSet(obj *fedv1b1.KubeFedCluster) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	klog.V(1).Infof("ClusterController observed a cluster deletion: %v", obj.Name)
	delete(cc.clusterDataMap, obj.Name)
}

// addToClusterSet creates a new client for the cluster and stores it in cluster data map.
func (cc *ClusterController) addToClusterSet(obj *fedv1b1.KubeFedCluster) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	clusterData := cc.clusterDataMap[obj.Name]
	if clusterData != nil && clusterData.clusterKubeClient != nil {
		return
	}

	klog.V(1).Infof("ClusterController observed a new cluster: %v", obj.Name)

	// create the restclient of cluster
	restClient, err := NewClusterClientSet(obj, cc.client, cc.fedNamespace, cc.clusterHealthCheckConfig.Timeout)
	if err != nil || restClient == nil {
		cc.RecordError(obj, "MalformedClusterConfig", errors.Wrap(err, "The configuration for this cluster may be malformed"))
		return
	}
	cc.clusterDataMap[obj.Name] = &ClusterData{clusterKubeClient: restClient, cachedObj: obj.DeepCopy()}
}

// Run begins watching and syncing.
func (cc *ClusterController) Run(stopChan <-chan struct{}) {
	defer utilruntime.HandleCrash()
	go cc.clusterController.Run(stopChan)
	// monitor cluster status periodically, in phase 1 we just get the health state from "/healthz"
	go wait.Until(func() {
		if err := cc.updateClusterStatus(); err != nil {
			klog.Errorf("Error monitoring cluster status: %v", err)
		}
	}, cc.clusterHealthCheckConfig.Period, stopChan)
}

// updateClusterStatus checks cluster health and updates status of all KubeFedClusters
func (cc *ClusterController) updateClusterStatus() error {
	clusters := &fedv1b1.KubeFedClusterList{}
	err := cc.client.List(context.TODO(), clusters, cc.fedNamespace)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	for _, obj := range clusters.Items {
		cc.mu.RLock()
		cluster := obj.DeepCopy()
		clusterData := cc.clusterDataMap[cluster.Name]
		cc.mu.RUnlock()
		if clusterData == nil {
			// Retry adding cluster client
			cc.addToClusterSet(cluster)
			cc.mu.RLock()
			clusterData = cc.clusterDataMap[cluster.Name]
			cc.mu.RUnlock()
			if clusterData == nil {
				klog.Warningf("Failed to retrieve stored data for cluster %s", cluster.Name)
				continue
			}
		}

		// 想起了曾曾经的一个面试题 哈哈哈😂 这里必须要传地址才能够调用done方法 否则永远不会退出
		wg.Add(1)
		go cc.updateIndividualClusterStatus(cluster, clusterData, &wg)
	}

	wg.Wait()
	return nil
}

func (cc *ClusterController) updateIndividualClusterStatus(cluster *fedv1b1.KubeFedCluster,
	storedData *ClusterData, wg *sync.WaitGroup) {
	defer metrics.ClusterHealthStatusDurationFromStart(time.Now())

	clusterClient := storedData.clusterKubeClient

	currentClusterStatus, err := clusterClient.GetClusterHealthStatus()
	if err != nil {
		cc.RecordError(cluster, "RetrievingClusterHealthFailed", errors.Wrap(err, "Failed to retrieve health of the cluster"))
	}

	currentClusterStatus = thresholdAdjustedClusterStatus(currentClusterStatus, storedData, cc.clusterHealthCheckConfig)

	if utilfeature.DefaultFeatureGate.Enabled(features.CrossClusterServiceDiscovery) {
		currentClusterStatus = cc.updateClusterZonesAndRegion(currentClusterStatus, cluster, clusterClient)
	}

	storedData.clusterStatus = currentClusterStatus
	cluster.Status = *currentClusterStatus
	if err := cc.client.UpdateStatus(context.TODO(), cluster); err != nil {
		klog.Warningf("Failed to update the status of cluster %q: %v", cluster.Name, err)
	}

	wg.Done()
}

func (cc *ClusterController) RecordError(cluster runtime.Object, errorCode string, err error) {
	cc.eventRecorder.Eventf(cluster, corev1.EventTypeWarning, errorCode, err.Error())
}

func thresholdAdjustedClusterStatus(clusterStatus *fedv1b1.KubeFedClusterStatus, storedData *ClusterData,
	clusterHealthCheckConfig *util.ClusterHealthCheckConfig) *fedv1b1.KubeFedClusterStatus {
	if storedData.clusterStatus == nil {
		storedData.resultRun = 1
		return clusterStatus
	}

	threshold := clusterHealthCheckConfig.FailureThreshold
	if util.IsClusterReady(clusterStatus) {
		threshold = clusterHealthCheckConfig.SuccessThreshold
	}

	if storedData.resultRun < threshold {
		// Success/Failure is below threshold - leave the probe state unchanged.
		probeTime := clusterStatus.Conditions[0].LastProbeTime
		clusterStatus = storedData.clusterStatus
		setProbeTime(clusterStatus, probeTime)
	} else if clusterStatusEqual(clusterStatus, storedData.clusterStatus) {
		// preserve the last transition time
		setTransitionTime(clusterStatus, *storedData.clusterStatus.Conditions[0].LastTransitionTime)
	}

	if clusterStatusEqual(clusterStatus, storedData.clusterStatus) {
		// Increment the result run has there is no change in cluster condition
		storedData.resultRun++
	} else {
		// Reset the result run
		storedData.resultRun = 1
	}

	return clusterStatus
}

func (cc *ClusterController) updateClusterZonesAndRegion(clusterStatus *fedv1b1.KubeFedClusterStatus, cluster *fedv1b1.KubeFedCluster,
	clusterClient *ClusterClient) *fedv1b1.KubeFedClusterStatus {
	if !util.IsClusterReady(clusterStatus) {
		return clusterStatus
	}

	zones, region, err := clusterClient.GetClusterZones()
	if err != nil {
		cc.RecordError(cluster, "RetrievingRegionZonesFailed", errors.Wrap(err, "Failed to get zones and region for the cluster"))
		return clusterStatus
	}

	// If new zone & region are empty, preserve the old ones so that user configured zone & region
	// labels are effective
	if len(zones) == 0 {
		zones = cluster.Status.Zones
	}
	if len(region) == 0 && cluster.Status.Region != nil {
		region = *cluster.Status.Region
	}
	clusterStatus.Zones = zones
	clusterStatus.Region = &region
	return clusterStatus
}

func clusterStatusEqual(newClusterStatus, oldClusterStatus *fedv1b1.KubeFedClusterStatus) bool {
	return util.IsClusterReady(newClusterStatus) == util.IsClusterReady(oldClusterStatus)
}

func setProbeTime(clusterStatus *fedv1b1.KubeFedClusterStatus, probeTime metav1.Time) {
	for i := 0; i < len(clusterStatus.Conditions); i++ {
		clusterStatus.Conditions[i].LastProbeTime = probeTime
	}
}

func setTransitionTime(clusterStatus *fedv1b1.KubeFedClusterStatus, transitionTime metav1.Time) {
	for i := 0; i < len(clusterStatus.Conditions); i++ {
		clusterStatus.Conditions[i].LastTransitionTime = &transitionTime
	}
}
