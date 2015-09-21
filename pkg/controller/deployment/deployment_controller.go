/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package deployment

import (
	"fmt"
	"math"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/experimental"
	"k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/client/record"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util"
	dutil "k8s.io/kubernetes/pkg/util/deployment"
	"k8s.io/kubernetes/pkg/util/workqueue"
	"k8s.io/kubernetes/pkg/watch"
)

const (
	// We'll attempt to recompute the required replicas of all deployments
	// that have fulfilled their expectations at least this often. This recomputation
	// happens based on contents in the local caches.
	FullDeploymentResyncPeriod = 30 * time.Second

	// We'll keep replication controller watches open up to this long. In the unlikely case
	// that a watch misdelivers info about an RC, it'll take this long for
	// that mistake to be rectified.
	ControllerRelistPeriod = 5 * time.Minute

	// We'll keep pod watches open up to this long. In the unlikely case
	// that a watch misdelivers info about a pod, it'll take this long for
	// that mistake to be rectified.
	PodRelistPeriod = 5 * time.Minute
)

type DeploymentController struct {
	client        client.Interface
	expClient     client.ExperimentalInterface
	eventRecorder record.EventRecorder
	rcControl     controller.RCControlInterface
	// To allow injection of syncDeployment for testing.
	syncHandler func(dKey string) error
	// A store of deployments, populated by the dController
	dStore cache.StoreToDeploymentLister
	// Watches changes to all deployments
	dController *framework.Controller
	// A store of replication controllers, populated by the rcController
	rcStore cache.StoreToReplicationControllerLister
	// Watches changes to all replication controllers
	rcController *framework.Controller
	// rcStoreSynced returns true if the RC store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	rcStoreSynced func() bool
	// A store of pods, populated by the podController
	podStore cache.StoreToPodLister
	// Watches changes to all pods
	podController *framework.Controller
	// podStoreSynced returns true if the pod store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	podStoreSynced func() bool
	// Deployments that need to be synced
	queue *workqueue.Type
}

func NewDeploymentController(client client.Interface) *DeploymentController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(client.Events(""))

	dc := &DeploymentController{
		client:        client,
		expClient:     client.Experimental(),
		eventRecorder: eventBroadcaster.NewRecorder(api.EventSource{Component: "deployment-controller"}),
		rcControl: controller.RealRCControl{
			KubeClient: client,
			Recorder:   eventBroadcaster.NewRecorder(api.EventSource{Component: "deployment-controller"}),
		},

		queue: workqueue.New(),
	}

	dc.dStore.Store, dc.dController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func() (runtime.Object, error) {
				return dc.expClient.Deployments(api.NamespaceAll).List(labels.Everything(), fields.Everything())
			},
			WatchFunc: func(rv string) (watch.Interface, error) {
				return dc.expClient.Deployments(api.NamespaceAll).Watch(labels.Everything(), fields.Everything(), rv)
			},
		},
		&experimental.Deployment{},
		FullDeploymentResyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc: dc.enqueueDeployment,
			UpdateFunc: func(old, cur interface{}) {
				// Resync on deployment object relist.
				dc.enqueueDeployment(cur)
			},
			// This will enter the sync loop and no-op, because the deployment has been deleted from the store.
			// Note that deleting a controller immediately after scaling it to 0 will not work. The recommended
			// way of achieving this is by performing a `stop` operation on the deployment.
			DeleteFunc: dc.enqueueDeployment,
		},
	)

	dc.rcStore.Store, dc.rcController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func() (runtime.Object, error) {
				return dc.client.ReplicationControllers(api.NamespaceAll).List(labels.Everything())
			},
			WatchFunc: func(rv string) (watch.Interface, error) {
				return dc.client.ReplicationControllers(api.NamespaceAll).Watch(labels.Everything(), fields.Everything(), rv)
			},
		},
		&api.ReplicationController{},
		ControllerRelistPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    dc.addRC,
			UpdateFunc: dc.updateRC,
			DeleteFunc: dc.deleteRC,
		},
	)

	// We do not event on anything from the podController, but we use the local
	// podStore to make queries about the current state of pods (e.g. whether
	// they are ready or not) more efficient.
	dc.podStore.Store, dc.podController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func() (runtime.Object, error) {
				return dc.client.Pods(api.NamespaceAll).List(labels.Everything(), fields.Everything())
			},
			WatchFunc: func(rv string) (watch.Interface, error) {
				return dc.client.Pods(api.NamespaceAll).Watch(labels.Everything(), fields.Everything(), rv)
			},
		},
		&api.Pod{},
		PodRelistPeriod,
		framework.ResourceEventHandlerFuncs{},
	)

	dc.syncHandler = dc.syncDeployment
	return dc
}

// When an RC is created, enqueue the deployment that manages it.
func (dc *DeploymentController) addRC(obj interface{}) {
	rc := obj.(*api.ReplicationController)
	if d := dc.getDeploymentForRC(rc); rc != nil {
		dc.enqueueDeployment(d)
	}
}

// getDeploymentForRC returns the deployment managing the given RC.
// TODO: Surface that we are ignoring multiple deployments for a given controller.
func (dc *DeploymentController) getDeploymentForRC(rc *api.ReplicationController) *experimental.Deployment {
	deployments, err := dc.dStore.GetDeploymentsForRC(rc)
	if err != nil {
		glog.V(4).Infof("No deployments found for replication controller %v, deployment controller will avoid syncing", rc.Name)
		return nil
	}
	// Because all RC's belonging to a deployment should have a unique label key,
	// there should never be more than one deployment returned by the above method.
	// If that happens we should probably dynamically repair the situation by ultimately
	// trying to clean up one of the controllers, for now we just return one of the two,
	// likely randomly.
	return &deployments[0]
}

// When a controller is updated, figure out what deployment/s manage it and wake them
// up. If the labels of the controller have changed we need to awaken both the old
// and new deployments. old and cur must be *api.ReplicationController types.
func (dc *DeploymentController) updateRC(old, cur interface{}) {
	if api.Semantic.DeepEqual(old, cur) {
		// A periodic relist will send update events for all known controllers.
		return
	}
	// TODO: Write a unittest for this case
	curRC := cur.(*api.ReplicationController)
	if d := dc.getDeploymentForRC(curRC); d != nil {
		dc.enqueueDeployment(d)
	}
	// A number of things could affect the old deployment: labels changing,
	// pod template changing, etc.
	oldRC := old.(*api.ReplicationController)
	// TODO: Is this the right way to check this, or is checking names sufficient?
	if !api.Semantic.DeepEqual(oldRC, curRC) {
		if oldD := dc.getDeploymentForRC(oldRC); oldD != nil {
			dc.enqueueDeployment(oldD)
		}
	}
}

// When a controller is deleted, enqueue the deployment that manages it.
// obj could be an *api.ReplicationController, or a DeletionFinalStateUnknown
// marker item.
func (dc *DeploymentController) deleteRC(obj interface{}) {
	rc, ok := obj.(*api.ReplicationController)

	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale. If the RC
	// changed labels the new deployment will not be woken up till the periodic resync.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			glog.Errorf("Couldn't get object from tombstone %+v, could take up to %v before a deployment recreates/updates controllers", obj, FullDeploymentResyncPeriod)
			return
		}
		rc, ok = tombstone.Obj.(*api.ReplicationController)
		if !ok {
			glog.Errorf("Tombstone contained object that is not an rc %+v, could take up to %v before a deployment recreates/updates controllers", obj, FullDeploymentResyncPeriod)
			return
		}
	}
	if d := dc.getDeploymentForRC(rc); d != nil {
		dc.enqueueDeployment(d)
	}
}

// obj could be an *api.Deployment, or a DeletionFinalStateUnknown marker item.
func (dc *DeploymentController) enqueueDeployment(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		glog.Errorf("Couldn't get key for object %+v: %v", obj, err)
		return
	}

	// TODO: Handle overlapping deployments better. Either disallow them at admission time or
	// deterministically avoid syncing deployments that fight over RC's. Currently, we only
	// ensure that the same deployment is synced for a given RC. When we periodically relist
	// all deployments there will still be some RC instability. One way to handle this is
	// by querying the store for all deployments that this deployment overlaps, as well as all
	// deployments that overlap this deployments, and sorting them.
	dc.queue.Add(key)
}

func (dc *DeploymentController) Run(workers int, stopCh <-chan struct{}) {
	defer util.HandleCrash()
	go dc.dController.Run(stopCh)
	go dc.rcController.Run(stopCh)
	go dc.podController.Run(stopCh)
	for i := 0; i < workers; i++ {
		go util.Until(dc.worker, time.Second, stopCh)
	}
	<-stopCh
	glog.Infof("Shutting down deployment controller")
	dc.queue.ShutDown()
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (dc *DeploymentController) worker() {
	for {
		func() {
			key, quit := dc.queue.Get()
			if quit {
				return
			}
			defer dc.queue.Done(key)
			err := dc.syncHandler(key.(string))
			if err != nil {
				glog.Errorf("Error syncing deployment: %v", err)
			}
		}()
	}
}

func (dc *DeploymentController) syncDeployment(key string) error {
	startTime := time.Now()
	defer func() {
		glog.V(4).Infof("Finished syncing deployment %q (%v)", key, time.Now().Sub(startTime))
	}()

	obj, exists, err := dc.dStore.Store.GetByKey(key)
	if !exists {
		glog.Infof("Deployment has been deleted %v", key)
		return nil
	}
	if err != nil {
		glog.Infof("Unable to retrieve deployment %v from store: %v", key, err)
		dc.queue.Add(key)
		return err
	}
	d := *obj.(*experimental.Deployment)
	switch d.Spec.Strategy.Type {
	case experimental.RecreateDeploymentStrategyType:
		return dc.syncRecreateDeployment(d)
	case experimental.RollingUpdateDeploymentStrategyType:
		return dc.syncRollingUpdateDeployment(d)
	}
	return fmt.Errorf("unexpected deployment strategy type: %s", d.Spec.Strategy.Type)
}

func (dc *DeploymentController) syncRecreateDeployment(deployment experimental.Deployment) error {
	// TODO: implement me.
	return nil
}

func (dc *DeploymentController) syncRollingUpdateDeployment(deployment experimental.Deployment) error {
	newRC, err := dc.getNewRC(deployment)
	if err != nil {
		return err
	}

	oldRCs, err := dutil.GetOldRCs(deployment, dc.client)
	if err != nil {
		return err
	}

	allRCs := append(oldRCs, newRC)

	// Scale up, if we can.
	scaledUp, err := dc.scaleUp(allRCs, newRC, deployment)
	if err != nil {
		return err
	}
	if scaledUp {
		// Update DeploymentStatus
		return dc.updateDeploymentStatus(allRCs, newRC, deployment)
	}

	// Scale down, if we can.
	scaledDown, err := dc.scaleDown(allRCs, oldRCs, newRC, deployment)
	if err != nil {
		return err
	}
	if scaledDown {
		// Update DeploymentStatus
		return dc.updateDeploymentStatus(allRCs, newRC, deployment)
	}
	// TODO: raise an event, neither scaled up nor down.
	return nil
}

// Returns an RC that matches the intent of the given deployment.
// It creates a new RC if required.
func (dc *DeploymentController) getNewRC(deployment experimental.Deployment) (*api.ReplicationController, error) {
	existing, err := dutil.GetNewRC(deployment, dc.client)
	if err != nil {
		return nil, fmt.Errorf("error finding new replication controller: %v", err)
	}
	if existing != nil {
		return existing, nil
	}
	// new RC does not exist, create one.
	newRCTemplate := dutil.GetNewRCTemplate(deployment)
	namespace := deployment.ObjectMeta.Namespace
	podTemplateSpecHash := dutil.GetPodTemplateSpecHash(deployment.Spec.Template)
	rcName := fmt.Sprintf("deploymentrc-%d", podTemplateSpecHash)
	newRC := api.ReplicationController{
		ObjectMeta: api.ObjectMeta{
			Name:      rcName,
			Namespace: namespace,
		},
		Spec: api.ReplicationControllerSpec{
			Replicas: 0,
			Selector: newRCTemplate.ObjectMeta.Labels,
			Template: newRCTemplate,
		},
	}
	createdRC, err := dc.client.ReplicationControllers(namespace).Create(&newRC)
	if err != nil {
		return nil, fmt.Errorf("error creating replication controller: %v", err)
	}
	return createdRC, nil
}

func (dc *DeploymentController) getReplicaCountForRCs(replicationControllers []*api.ReplicationController) int {
	totalReplicaCount := 0
	for _, rc := range replicationControllers {
		totalReplicaCount += rc.Spec.Replicas
	}
	return totalReplicaCount
}

func (dc *DeploymentController) scaleUp(allRCs []*api.ReplicationController, newRC *api.ReplicationController, deployment experimental.Deployment) (bool, error) {
	if newRC.Spec.Replicas == deployment.Spec.Replicas {
		// Scaling up not required.
		return false, nil
	}
	maxSurge, isPercent, err := util.GetIntOrPercentValue(&deployment.Spec.Strategy.RollingUpdate.MaxSurge)
	if err != nil {
		return false, fmt.Errorf("invalid value for MaxSurge: %v", err)
	}
	if isPercent {
		maxSurge = util.GetValueFromPercent(maxSurge, deployment.Spec.Replicas)
	}
	currentPodCount := dutil.GetReplicaCountForRCs(allRCs)
	// Check if we can scale up.
	maxTotalPods := deployment.Spec.Replicas + maxSurge
	if currentPodCount >= maxTotalPods {
		// Cannot scale up.
		return false, nil
	}
	// Scale up.
	scaleUpCount := maxTotalPods - currentPodCount
	scaleUpCount = int(math.Min(float64(scaleUpCount), float64(deployment.Spec.Replicas-newRC.Spec.Replicas)))
	newReplicasCount := newRC.Spec.Replicas + scaleUpCount
	_, err = dc.scaleRC(newRC, newReplicasCount)
	if err == nil {
		dc.eventRecorder.Eventf(&deployment, "ScalingRC", "Scaled up rc %s to %d", newRC.Name, newReplicasCount)
	}
	return true, err
}

func (dc *DeploymentController) scaleDown(allRCs []*api.ReplicationController, oldRCs []*api.ReplicationController, newRC *api.ReplicationController, deployment experimental.Deployment) (bool, error) {
	oldPodsCount := dc.getReplicaCountForRCs(oldRCs)
	if oldPodsCount == 0 {
		// Cant scale down further
		return false, nil
	}
	maxUnavailable, isPercent, err := util.GetIntOrPercentValue(&deployment.Spec.Strategy.RollingUpdate.MaxUnavailable)
	if err != nil {
		return false, fmt.Errorf("invalid value for MaxUnavailable: %v", err)
	}
	if isPercent {
		maxUnavailable = util.GetValueFromPercent(maxUnavailable, deployment.Spec.Replicas)
	}
	// Check if we can scale down.
	minAvailable := deployment.Spec.Replicas - maxUnavailable
	// Find the number of ready pods.
	readyPodCount, err := dutil.GetAvailablePodsForRCs(dc.client, allRCs)
	if err != nil {
		return false, fmt.Errorf("could not find available pods: %v", err)
	}

	if readyPodCount <= minAvailable {
		// Cannot scale down.
		return false, nil
	}
	totalScaleDownCount := readyPodCount - minAvailable
	for _, targetRC := range oldRCs {
		if totalScaleDownCount == 0 {
			// No further scaling required.
			break
		}
		if targetRC.Spec.Replicas == 0 {
			// cannot scale down this RC.
			continue
		}
		// Scale down.
		scaleDownCount := int(math.Min(float64(targetRC.Spec.Replicas), float64(totalScaleDownCount)))
		newReplicasCount := targetRC.Spec.Replicas - scaleDownCount
		_, err = dc.scaleRC(targetRC, newReplicasCount)
		if err != nil {
			return false, err
		}
		dc.eventRecorder.Eventf(&deployment, "ScalingRC", "Scaled down rc %s to %d", targetRC.Name, newReplicasCount)
		totalScaleDownCount -= scaleDownCount
	}
	return true, err
}

func (dc *DeploymentController) updateDeploymentStatus(allRCs []*api.ReplicationController, newRC *api.ReplicationController, deployment experimental.Deployment) error {
	totalReplicas := dc.getReplicaCountForRCs(allRCs)
	updatedReplicas := dc.getReplicaCountForRCs([]*api.ReplicationController{newRC})
	newDeployment := deployment
	// TODO: Reconcile this with API definition. API definition talks about ready pods, while this just computes created pods.
	newDeployment.Status = experimental.DeploymentStatus{
		Replicas:        totalReplicas,
		UpdatedReplicas: updatedReplicas,
	}
	_, err := dc.updateDeployment(&newDeployment)
	return err
}

func (dc *DeploymentController) scaleRC(rc *api.ReplicationController, newScale int) (*api.ReplicationController, error) {
	// TODO: Using client for now, update to use store when it is ready.
	rc.Spec.Replicas = newScale
	return dc.client.ReplicationControllers(rc.ObjectMeta.Namespace).Update(rc)
}

func (dc *DeploymentController) updateDeployment(deployment *experimental.Deployment) (*experimental.Deployment, error) {
	// TODO: Using client for now, update to use store when it is ready.
	return dc.client.Experimental().Deployments(deployment.ObjectMeta.Namespace).Update(deployment)
}
