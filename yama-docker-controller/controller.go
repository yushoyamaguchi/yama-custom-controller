/*
Copyright 2017 The Kubernetes Authors.

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

package main

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"sync"
	"time"

	"golang.org/x/time/rate"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	samplev1alpha1 "k8s.io/sample-controller/pkg/apis/samplecontroller/v1alpha1"
	clientset "k8s.io/sample-controller/pkg/generated/clientset/versioned"
	samplescheme "k8s.io/sample-controller/pkg/generated/clientset/versioned/scheme"
	informers "k8s.io/sample-controller/pkg/generated/informers/externalversions/samplecontroller/v1alpha1"
	listers "k8s.io/sample-controller/pkg/generated/listers/samplecontroller/v1alpha1"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

const controllerAgentName = "sample-controller"

const (
	// SuccessSynced is used as part of the Event 'reason' when a Foo is synced
	SuccessSynced = "Synced"
	// ErrResourceExists is used as part of the Event 'reason' when a Foo fails
	// to sync due to a Deployment of the same name already existing.
	ErrResourceExists = "ErrResourceExists"

	// MessageResourceExists is the message used for Events when a resource
	// fails to sync due to a Deployment already existing
	MessageResourceExists = "Resource %q already exists and is not managed by Foo"
	// MessageResourceSynced is the message used for an Event fired when a Foo
	// is synced successfully
	MessageResourceSynced = "Foo synced successfully"
	// FieldManager distinguishes this controller from other things writing to API objects
	FieldManager = controllerAgentName
)

// Controller is the controller implementation for Foo resources
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	// sampleclientset is a clientset for our own API group
	sampleclientset clientset.Interface

	yamaDockersLister listers.YamaDockerLister
	yamaDockersSynced cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.TypedRateLimitingInterface[cache.ObjectName]
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder     record.EventRecorder
	dockerClient *client.Client

	yamaDockerToContainerName map[string]string
	mapMutex                  sync.RWMutex
}

// NewController returns a new sample controller
func NewController(
	ctx context.Context,
	kubeclientset kubernetes.Interface,
	sampleclientset clientset.Interface,
	yamaDockerInformer informers.YamaDockerInformer) *Controller {
	logger := klog.FromContext(ctx)

	// Create event broadcaster
	// Add sample-controller types to the default Kubernetes Scheme so Events can be
	// logged for sample-controller types.
	utilruntime.Must(samplescheme.AddToScheme(scheme.Scheme))
	logger.V(4).Info("Creating event broadcaster")

	eventBroadcaster := record.NewBroadcaster(record.WithContext(ctx))
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})
	ratelimiter := workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[cache.ObjectName](5*time.Millisecond, 1000*time.Second),
		&workqueue.TypedBucketRateLimiter[cache.ObjectName]{Limiter: rate.NewLimiter(rate.Limit(50), 300)},
	)

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.Error(err, "Failed to create Docker client")
		return nil
	}

	controller := &Controller{
		kubeclientset:             kubeclientset,
		sampleclientset:           sampleclientset,
		yamaDockersLister:         yamaDockerInformer.Lister(),
		yamaDockersSynced:         yamaDockerInformer.Informer().HasSynced,
		workqueue:                 workqueue.NewTypedRateLimitingQueue(ratelimiter),
		recorder:                  recorder,
		dockerClient:              dockerClient,
		yamaDockerToContainerName: make(map[string]string),
	}

	logger.Info("Setting up event handlers")
	// Set up an event handler for when Foo resources change
	yamaDockerInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueFoo,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueFoo(new)
		},
		DeleteFunc: controller.enqueueFoo,
	})

	// Start watching Docker events
	go controller.watchDockerEvents(ctx)

	return controller
}

// Add these methods to manage the mapping
func (c *Controller) addMapping(yamaDockerName, containerName string) {
	c.mapMutex.Lock()
	defer c.mapMutex.Unlock()
	c.yamaDockerToContainerName[yamaDockerName] = containerName
}

func (c *Controller) removeMapping(yamaDockerName string) {
	c.mapMutex.Lock()
	defer c.mapMutex.Unlock()
	delete(c.yamaDockerToContainerName, yamaDockerName)
}

func (c *Controller) getContainerName(yamaDockerName string) (string, bool) {
	c.mapMutex.RLock()
	defer c.mapMutex.RUnlock()
	containerName, exists := c.yamaDockerToContainerName[yamaDockerName]
	return containerName, exists
}

func (c *Controller) watchDockerEvents(ctx context.Context) {
	logger := klog.FromContext(ctx)

	filters := filters.NewArgs()
	filters.Add("type", "container")

	options := events.ListOptions{
		Filters: filters,
	}

	messages, errs := c.dockerClient.Events(ctx, options)

	for {
		select {
		case event := <-messages:
			// Handle the event
			logger.V(4).Info("Received Docker event", "event", event)

			// Enqueue the relevant YamaDocker resource
			c.enqueueYamaDockerForEvent(event)
		case err := <-errs:
			logger.Error(err, "Error from Docker events")
		case <-ctx.Done():
			logger.Info("Stopping Docker event watcher")
			return
		}
	}
}

func (c *Controller) enqueueYamaDockerForEvent(event events.Message) {
	containerName := event.Actor.Attributes["name"]
	if containerName == "" {
		return
	}

	// Find the YamaDocker resource with this containerName
	yamaDockers, err := c.yamaDockersLister.List(labels.Everything())
	if err != nil {
		klog.Error(err, "Failed to list YamaDocker resources")
		return
	}

	for _, yamaDocker := range yamaDockers {
		if yamaDocker.Spec.ContainerName == containerName {
			objectRef := cache.ObjectName{
				Namespace: yamaDocker.Namespace,
				Name:      yamaDocker.Name,
			}
			c.workqueue.Add(objectRef)
		}
	}
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()
	logger := klog.FromContext(ctx)

	// Start the informer factories to begin populating the informer caches
	logger.Info("Starting Foo controller")

	// Wait for the caches to be synced before starting workers
	logger.Info("Waiting for informer caches to sync")

	if ok := cache.WaitForCacheSync(ctx.Done(), c.yamaDockersSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logger.Info("Starting workers", "count", workers)
	// Launch two workers to process Foo resources
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	logger.Info("Started workers")
	<-ctx.Done()
	logger.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	objRef, shutdown := c.workqueue.Get()
	logger := klog.FromContext(ctx)

	if shutdown {
		return false
	}

	// We call Done at the end of this func so the workqueue knows we have
	// finished processing this item. We also must remember to call Forget
	// if we do not want this work item being re-queued. For example, we do
	// not call Forget if a transient error occurs, instead the item is
	// put back on the workqueue and attempted again after a back-off
	// period.
	defer c.workqueue.Done(objRef)

	// Run the syncHandler, passing it the structured reference to the object to be synced.
	err := c.syncHandler(ctx, objRef)
	if err == nil {
		// If no error occurs then we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(objRef)
		logger.Info("Successfully synced", "objectName", objRef)
		return true
	}
	// there was a failure so be sure to report it.  This method allows for
	// pluggable error handling which can be used for things like
	// cluster-monitoring.
	utilruntime.HandleErrorWithContext(ctx, err, "Error syncing; requeuing for later retry", "objectReference", objRef)
	// since we failed, we should requeue the item to work on later.  This
	// method will add a backoff to avoid hotlooping on particular items
	// (they're probably still not going to work right away) and overall
	// controller protection (everything I've done is broken, this controller
	// needs to calm down or it can starve other useful work) cases.
	c.workqueue.AddRateLimited(objRef)
	return true
}

func (c *Controller) syncHandler(ctx context.Context, objectRef cache.ObjectName) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "objectRef", objectRef)

	// Get the YamaDocker resource
	yamaDocker, err := c.yamaDockersLister.YamaDockers(objectRef.Namespace).Get(objectRef.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleErrorWithContext(ctx, err, "YamaDocker resource not found", "objectRef", objectRef)
			err = c.cleanupContainer(ctx, objectRef)
			if err != nil {
				logger.Error(err, "Failed to cleanup Docker container", "objectRef", objectRef)
			}
			return nil
		}
		return err
	}

	containerName := yamaDocker.Spec.ContainerName

	// Check the current state of the Docker container
	containers, err := c.dockerClient.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", containerName), filters.Arg("status", "running"), filters.Arg("label", "created_by=YamaDocker-Controller")),
	})

	if err != nil {
		logger.Error(err, "Failed to list Docker containers")
		return err
	}

	fmt.Println("yama_debug: len(containers): ", len(containers))

	if len(containers) == 0 {
		// Container does not exist; create and start it
		logger.Info("Container not found, creating", "containerName", containerName)
		err = c.recreateContainer(ctx, "", yamaDocker)
		if err != nil {
			logger.Error(err, "Failed to recreate container", "containerName", containerName)
			return err
		}
	} else {
		// Ensure the container is running
		dockerContainer := containers[0]
		if dockerContainer.State != "running" {
			logger.Info("Container is not running, starting", "containerName", containerName)
			err = c.dockerClient.ContainerStart(ctx, dockerContainer.ID, container.StartOptions{})
			logger.Error(err, "Failed to start container1", "containerName", containerName)
			return err
		}
	}

	// Update the status of the YamaDocker resource
	err = c.updateYamaDockerStatus(ctx, yamaDocker)
	if err != nil {
		return err
	}

	c.recorder.Event(yamaDocker, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	return nil
}

func (c *Controller) createAndStartContainer(ctx context.Context, yamaDocker *samplev1alpha1.YamaDocker) error {
	containerName := yamaDocker.Spec.ContainerName
	imageName := yamaDocker.Spec.ImageName

	logger := klog.LoggerWithValues(klog.FromContext(ctx), "containerName", containerName)

	// Pull the image
	out, err := c.dockerClient.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		logger.Error(err, "Failed to pull image", "imageName", imageName)
		return err
	}
	defer out.Close()
	io.Copy(io.Discard, out) // Read the output to completion

	// Create the container
	resp, err := c.dockerClient.ContainerCreate(ctx, &container.Config{
		Image: imageName,
		Cmd:   []string{"sleep", "infinity"},
		Labels: map[string]string{
			"created_by": "YamaDocker-Controller",
		},
	}, nil, nil, nil, containerName)
	if err != nil {
		logger.Error(err, "Failed to create container")
		return err
	}

	// Start the container
	err = c.dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{})
	if err != nil {
		logger.Error(err, "Failed to start container2")
		return err
	}

	c.addMapping(yamaDocker.Name, containerName)

	logger.Info("Container created and started", "containerID", resp.ID)
	return nil
}

func (c *Controller) recreateContainer(ctx context.Context, containerID string, yamaDocker *samplev1alpha1.YamaDocker) error {
	containerName := yamaDocker.Spec.ContainerName

	logger := klog.LoggerWithValues(klog.FromContext(ctx), "containerName", containerName)

	// Stop the container
	err := c.dockerClient.ContainerStop(ctx, containerID, container.StopOptions{})
	if err != nil {
		if client.IsErrNotFound(err) {
			logger.Info("Container not found, skipping stop", "containerID", containerID)
		} else {
			logger.Error(err, "Failed to stop container", "containerID", containerID)
			return err
		}
	}

	// Remove the container
	err = c.dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{})
	if err != nil {
		if client.IsErrNotFound(err) {
			logger.Info("Container not found, skipping remove", "containerID", containerID)
		} else {
			logger.Error(err, "Failed to remove container", "containerID", containerID)
			return err
		}
	}

	// Create and start a new container
	err = c.createAndStartContainer(ctx, yamaDocker)
	if err != nil {
		return err
	}

	return nil
}

func (c *Controller) updateYamaDockerStatus(ctx context.Context, yamaDocker *samplev1alpha1.YamaDocker) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "yamaDocker", yamaDocker.Name)

	containerName := yamaDocker.Spec.ContainerName

	// Get the container status
	containers, err := c.dockerClient.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", containerName)),
	})

	if err != nil {
		logger.Error(err, "Failed to list Docker containers")
		return err
	}

	status := samplev1alpha1.YamaDockerStatus{}

	if len(containers) > 0 {
		containerJSON, err := c.dockerClient.ContainerInspect(ctx, containers[0].ID)
		if err != nil {
			logger.Error(err, "Failed to inspect container")
			return err
		}
		status.ContainerID = containerJSON.ID
		status.ContainerStatus = containerJSON.State.Status
	} else {
		status.ContainerID = ""
		status.ContainerStatus = "NotFound"
	}

	if !reflect.DeepEqual(yamaDocker.Status, status) {
		yamaDockerCopy := yamaDocker.DeepCopy()
		yamaDockerCopy.Status = status

		_, err := c.sampleclientset.SamplecontrollerV1alpha1().YamaDockers(yamaDocker.Namespace).UpdateStatus(ctx, yamaDockerCopy, metav1.UpdateOptions{FieldManager: FieldManager})
		if err != nil {
			logger.Error(err, "Failed to update YamaDocker status")
			return err
		}
	}

	return nil
}

// enqueueFoo takes a Foo resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than Foo.
func (c *Controller) enqueueFoo(obj interface{}) {
	if objectRef, err := cache.ObjectToName(obj); err != nil {
		utilruntime.HandleError(err)
		return
	} else {
		c.workqueue.Add(objectRef)
	}
}

// Cleanup function to stop and remove the Docker container when YamaDocker resource is deleted
func (c *Controller) cleanupContainer(ctx context.Context, objectRef cache.ObjectName) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "objectRef", objectRef)

	containerName, exists := c.getContainerName(objectRef.Name)
	if !exists {
		logger.Info("No container mapping found for cleanup", "objectRef", objectRef)
		return nil
	}

	// Find the Docker container associated with the YamaDocker resource
	containers, err := c.dockerClient.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "created_by=YamaDocker-Controller"), filters.Arg("name", containerName)),
	})

	fmt.Println("yama_debug: len(containers): ", len(containers))

	if err != nil {
		logger.Error(err, "Failed to list Docker containers for cleanup")
		return err
	}

	if len(containers) > 0 {
		// ToDo: check container name which should be deleted
		dockerContainer := containers[0]
		logger.Info("Stopping and removing container", "containerID", dockerContainer.ID)

		// Stop the container
		err = c.dockerClient.ContainerStop(ctx, dockerContainer.ID, container.StopOptions{})
		if err != nil {
			logger.Error(err, "Failed to stop container", "containerID", dockerContainer.ID)
			return err
		}

		// Remove the container
		err = c.dockerClient.ContainerRemove(ctx, dockerContainer.ID, container.RemoveOptions{Force: true})
		if err != nil {
			logger.Error(err, "Failed to remove container", "containerID", dockerContainer.ID)
			return err
		}

		logger.Info("Successfully cleaned up container", "containerID", dockerContainer.ID)
	} else {
		logger.Info("No container found for cleanup", "objectRef", objectRef)
	}

	c.removeMapping(objectRef.Name)

	return nil
}
