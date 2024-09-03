package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

var (
	myResourceGVR = schema.GroupVersionResource{
		Group:    "example.com",
		Version:  "v1",
		Resource: "myresources",
	}
)

type Controller struct {
	dynamicClient dynamic.Interface
	queue         workqueue.RateLimitingInterface
	informer      cache.SharedIndexInformer
}

func NewController(dynamicClient dynamic.Interface) *Controller {
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return dynamicClient.Resource(myResourceGVR).Namespace(metav1.NamespaceAll).List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return dynamicClient.Resource(myResourceGVR).Namespace(metav1.NamespaceAll).Watch(context.TODO(), options)
			},
		},
		&unstructured.Unstructured{},
		0,
		cache.Indexers{},
	)

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			queue.Add(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			queue.Add(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			queue.Add(obj)
		},
	})

	return &Controller{
		dynamicClient: dynamicClient,
		queue:         queue,
		informer:      informer,
	}
}

func (c *Controller) Run(stopCh chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	go c.informer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}

	for {
		obj, shutdown := c.queue.Get()
		if shutdown {
			break
		}

		if err := c.processItem(obj); err != nil {
			utilruntime.HandleError(err)
		}
		c.queue.Done(obj)
	}
}

func (c *Controller) processItem(obj interface{}) error {
	resource := obj.(*unstructured.Unstructured)
	fmt.Printf("Processing MyResource: %s/%s\n", resource.GetNamespace(), resource.GetName())

	unstructuredObj, err := c.dynamicClient.Resource(myResourceGVR).Namespace(resource.GetNamespace()).Get(context.TODO(), resource.GetName(), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get MyResource: %v", err)
	}

	name, _, _ := unstructured.NestedString(unstructuredObj.Object, "spec", "name")
	configKey, _, _ := unstructured.NestedString(unstructuredObj.Object, "spec", "configKey")
	configValue, _, _ := unstructured.NestedString(unstructuredObj.Object, "spec", "configValue")

	configMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: resource.GetNamespace(),
		},
		Data: map[string]string{
			configKey: configValue,
		},
	}

	configMapUnstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(configMap)
	if err != nil {
		return fmt.Errorf("failed to convert ConfigMap to Unstructured: %v", err)
	}

	_, err = c.dynamicClient.Resource(schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "configmaps",
	}).Namespace(resource.GetNamespace()).Create(context.TODO(), &unstructured.Unstructured{Object: configMapUnstructured}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create configmap: %v", err)
	}

	return nil
}

func main() {
	klog.InitFlags(nil)
	homeDir := os.Getenv("HOME")
	kubeconfig := filepath.Join(homeDir, ".kube", "config")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		klog.Fatalf("Error building kubeconfig: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Error building dynamic client: %v", err)
	}

	controller := NewController(dynamicClient)

	stopCh := make(chan struct{})
	defer close(stopCh)

	go controller.Run(stopCh)

	<-stopCh
}
