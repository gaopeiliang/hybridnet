/*
 Copyright 2021 The Hybridnet Authors.

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

package remotecluster

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	runtimeutil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	networkingv1 "github.com/alibaba/hybridnet/pkg/apis/networking/v1"
	"github.com/alibaba/hybridnet/pkg/client/clientset/versioned"
	"github.com/alibaba/hybridnet/pkg/client/informers/externalversions"
	informers "github.com/alibaba/hybridnet/pkg/client/informers/externalversions/networking/v1"
	listers "github.com/alibaba/hybridnet/pkg/client/listers/networking/v1"
	"github.com/alibaba/hybridnet/pkg/controller/remotecluster/lock"
	rctypes "github.com/alibaba/hybridnet/pkg/controller/remotecluster/types"
	"github.com/alibaba/hybridnet/pkg/rcmanager"
	"github.com/alibaba/hybridnet/pkg/utils"
)

const (
	ControllerName = "remotecluster"

	// HealthCheckPeriod Every HealthCheckPeriod will resync remote cluster cache and check rc
	// health. Default: 30 second. Set to zero will also use the default value
	HealthCheckPeriod = 30 * time.Second
)

type Controller struct {
	sync.Mutex
	hasSynced bool

	// localCluster's UUID
	UUID           types.UID
	OverlayNetID   *uint32
	overlayNetIDMU sync.RWMutex

	rcManagerCache sync.Map

	kubeClient                kubeclientset.Interface
	hybridnetClient           versioned.Interface
	HybridnetInformerFactory  externalversions.SharedInformerFactory
	remoteClusterLister       listers.RemoteClusterLister
	remoteClusterSynced       cache.InformerSynced
	remoteClusterQueue        workqueue.RateLimitingInterface
	remoteSubnetLister        listers.RemoteSubnetLister
	remoteSubnetSynced        cache.InformerSynced
	remoteVtepLister          listers.RemoteVtepLister
	remoteVtepSynced          cache.InformerSynced
	localClusterSubnetLister  listers.SubnetLister
	localClusterSubnetSynced  cache.InformerSynced
	localClusterNetworkLister listers.NetworkLister
	localClusterNetworkSynced cache.InformerSynced

	remoteClusterUUIDLock lock.UUIDLock

	remoteClusterEvent chan rctypes.Event

	recorder record.EventRecorder
}

func NewController(
	kubeClient kubeclientset.Interface,
	hybridnetClient versioned.Interface,
	remoteClusterInformer informers.RemoteClusterInformer,
	remoteSubnetInformer informers.RemoteSubnetInformer,
	localClusterSubnetInformer informers.SubnetInformer,
	remoteVtepInformer informers.RemoteVtepInformer,
	localClusterNetworkInformer informers.NetworkInformer) *Controller {
	runtimeutil.Must(networkingv1.AddToScheme(scheme.Scheme))

	klog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: ControllerName})

	uuid, err := utils.GetUUID(kubeClient)
	if err != nil {
		panic(err)
	}

	c := &Controller{
		Mutex:                     sync.Mutex{},
		hasSynced:                 false,
		rcManagerCache:            sync.Map{},
		UUID:                      uuid,
		kubeClient:                kubeClient,
		hybridnetClient:           hybridnetClient,
		remoteClusterLister:       remoteClusterInformer.Lister(),
		remoteClusterSynced:       remoteClusterInformer.Informer().HasSynced,
		remoteSubnetLister:        remoteSubnetInformer.Lister(),
		remoteSubnetSynced:        remoteSubnetInformer.Informer().HasSynced,
		localClusterSubnetLister:  localClusterSubnetInformer.Lister(),
		localClusterSubnetSynced:  localClusterSubnetInformer.Informer().HasSynced,
		remoteVtepLister:          remoteVtepInformer.Lister(),
		remoteVtepSynced:          remoteSubnetInformer.Informer().HasSynced,
		localClusterNetworkLister: localClusterNetworkInformer.Lister(),
		localClusterNetworkSynced: localClusterNetworkInformer.Informer().HasSynced,
		remoteClusterQueue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), ControllerName),
		remoteClusterUUIDLock:     lock.NewUUIDLock(),
		remoteClusterEvent:        make(chan rctypes.Event, 10),
		recorder:                  recorder,
	}

	remoteClusterInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: c.filterRemoteCluster,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    c.addOrDelRemoteCluster,
			UpdateFunc: c.updateRemoteCluster,
			DeleteFunc: c.addOrDelRemoteCluster,
		},
	})

	localClusterNetworkInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			network, ok := obj.(*networkingv1.Network)
			if !ok {
				return false
			}
			return network.Spec.Type == networkingv1.NetworkTypeOverlay
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(_ interface{}) {
				c.syncLocalOverlayNetIDOnce()
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldNetwork, _ := oldObj.(*networkingv1.Network)
				newNetwork, _ := newObj.(*networkingv1.Network)
				needSync := oldNetwork.Spec.Type != newNetwork.Spec.Type || oldNetwork.Spec.NetID == nil ||
					newNetwork.Spec.NetID == nil || *oldNetwork.Spec.NetID != *newNetwork.Spec.NetID

				if needSync {
					c.syncLocalOverlayNetIDOnce()
				}
			},
			DeleteFunc: func(_ interface{}) {
				c.syncLocalOverlayNetIDOnce()
			},
		},
	})

	return c
}

func (c *Controller) Run(stopCh <-chan struct{}) error {
	defer runtimeutil.HandleCrash()
	defer c.remoteClusterQueue.ShutDown()

	klog.Infof("Starting %s controller", ControllerName)

	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.remoteClusterSynced, c.remoteSubnetSynced, c.remoteVtepSynced, c.localClusterSubnetSynced, c.localClusterNetworkSynced); !ok {
		return fmt.Errorf("%s failed to wait for caches to sync", ControllerName)
	}

	c.Mutex.Lock()
	c.hasSynced = true
	c.Mutex.Unlock()

	// init UUID lock
	remoteClusterList, err := c.hybridnetClient.NetworkingV1().RemoteClusters().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range remoteClusterList.Items {
		var remoteCluster = remoteClusterList.Items[i]
		if len(remoteCluster.Status.UUID) > 0 {
			_ = c.remoteClusterUUIDLock.LockByOwner(remoteCluster.Status.UUID, remoteCluster.Name)
		}
	}

	// start workers
	klog.Info("Starting workers")
	go wait.Until(c.runRemoteClusterWorker, time.Second, stopCh)
	go wait.Until(c.updateAllRemoteClusterStatus, HealthCheckPeriod, stopCh)
	go wait.Until(c.handleEventFromRemoteClusters, time.Second, stopCh)

	<-stopCh

	c.closeAllRemoteClusterManager()

	klog.Info("Shutting down workers")
	return nil
}

func (c *Controller) closeAllRemoteClusterManager() {
	c.rcManagerCache.Range(func(_, value interface{}) bool {
		if manager, ok := value.(*rcmanager.Manager); ok {
			manager.Close()
		}
		return true
	})
}

func (c *Controller) syncLocalOverlayNetIDOnce() {
	c.overlayNetIDMU.Lock()
	defer c.overlayNetIDMU.Unlock()

	networks, err := c.localClusterNetworkLister.List(labels.Everything())
	if err != nil {
		klog.Warningf("[remote cluster] failed to list networks: %v", err)
		return
	}

	var overlayNetworkExist = false
	for _, network := range networks {
		if network.Spec.Type == networkingv1.NetworkTypeOverlay {
			overlayNetworkExist = true
			switch {
			case c.OverlayNetID == nil || network.Spec.NetID == nil:
				fallthrough
			case *c.OverlayNetID != *network.Spec.NetID:
				c.OverlayNetID = copyUint32Ptr(network.Spec.NetID)
			}
			break
		}
	}

	// clean overlay netID cache if non-existence
	if !overlayNetworkExist {
		c.OverlayNetID = nil
	}
}

// health checking and resync cache. remote cluster is managed by admin, it can be
// treated as desired states
func (c *Controller) updateAllRemoteClusterStatus() {
	remoteClusters, err := c.remoteClusterLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Can't list remote cluster. err=%v", err)
		return
	}

	var wg sync.WaitGroup
	for _, rc := range remoteClusters {
		r := rc.DeepCopy()

		managerObject, ok := c.rcManagerCache.Load(r.Name)
		if !ok {
			continue
		}
		manager, ok := managerObject.(*rcmanager.Manager)
		if !ok {
			continue
		}

		wg.Add(1)
		go func() {
			updateSingleRemoteClusterStatus(c, manager, r)
			wg.Done()
		}()
	}
	wg.Wait()
}

func (c *Controller) handleEventFromRemoteClusters() {
	for event := range c.remoteClusterEvent {
		switch event.Type {
		case rctypes.EventRefreshUUID:
			uuid, ok := event.Object.(types.UID)
			if !ok {
				klog.Warningf("[remote cluster] invalid object of remote cluster event")
				break
			}
			if len(event.ClusterName) == 0 {
				klog.Warningf("[remote cluster] invalid cluster name for remote cluster event")
				break
			}
			if err := c.remoteClusterUUIDLock.LockByOwner(uuid, event.ClusterName); err != nil {
				klog.Errorf("[remote cluster] uuid lock failed: %v", err)
				continue
			}

			_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				return c.patchUUIDtoRemoteCluster(event.ClusterName, uuid)
			})
			klog.Infof("[remote cluster] receive event and update UUID %s for cluster %s", uuid, event.ClusterName)

		case rctypes.EventUpdateStatus:
			if len(event.ClusterName) == 0 {
				klog.Warningf("invalid cluster for remote cluster event")
				break
			}

			remoteCluster, err := c.remoteClusterLister.Get(event.ClusterName)
			if err != nil {
				klog.Errorf("update status event fail on getting object: %v", err)
				break
			}
			remoteCluster = remoteCluster.DeepCopy()

			managerObject, ok := c.rcManagerCache.Load(event.ClusterName)
			if !ok {
				break
			}

			go updateSingleRemoteClusterStatus(c, managerObject.(*rcmanager.Manager), remoteCluster)
			klog.Infof("[remote cluster] receive event and update status for cluster %s", event.ClusterName)
		case rctypes.EventRecordEvent:
			if len(event.ClusterName) == 0 {
				klog.Warningf("invalid cluster for record event event")
				break
			}

			eventBody, ok := event.Object.(rctypes.EventBody)
			if !ok {
				break
			}

			remoteCluster, err := c.remoteClusterLister.Get(event.ClusterName)
			if err != nil {
				klog.Errorf("record event fail on getting object: %v", err)
				break
			}

			c.recorder.Event(remoteCluster, eventBody.EventType, eventBody.Reason, eventBody.Message)
			klog.Infof("[remote cluster] record event %v for cluster %s", eventBody, event.ClusterName)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (c *Controller) patchUUIDtoRemoteCluster(clusterName string, uuid types.UID) error {
	patchBody := fmt.Sprintf(
		`{"status":{"uuid":%q}}`,
		uuid,
	)

	_, err := c.hybridnetClient.NetworkingV1().RemoteClusters().Patch(context.TODO(), clusterName, types.MergePatchType, []byte(patchBody), metav1.PatchOptions{}, "status")
	return err
}

func copyUint32Ptr(i *uint32) *uint32 {
	if i == nil {
		return nil
	}
	o := *i
	return &o
}

func (c *Controller) GetUUID() types.UID {
	return c.UUID
}

func (c *Controller) Lock(uuid types.UID, clusterName string) error {
	return c.remoteClusterUUIDLock.LockByOwner(uuid, clusterName)
}

func (c *Controller) GetOverlayNetID() *uint32 {
	c.overlayNetIDMU.RLock()
	defer c.overlayNetIDMU.RUnlock()

	return c.OverlayNetID
}

func (c *Controller) ListSubnet() ([]*networkingv1.Subnet, error) {
	if !c.hasSynced {
		return nil, fmt.Errorf("informer cache has not synced yet")
	}
	return c.localClusterSubnetLister.List(labels.Everything())
}