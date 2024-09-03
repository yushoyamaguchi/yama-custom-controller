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
	configMapGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "configmaps",
	}
)

type Controller struct {
	dynamicClient      dynamic.Interface
	queue              workqueue.RateLimitingInterface
	myResourceInformer cache.SharedIndexInformer
	configMapInformer  cache.SharedIndexInformer
}

func NewController(dynamicClient dynamic.Interface) *Controller {
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	// MyResource Informer
	myResourceInformer := cache.NewSharedIndexInformer(
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

	myResourceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
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

	// ConfigMap Informer
	configMapInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return dynamicClient.Resource(configMapGVR).Namespace(metav1.NamespaceAll).List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return dynamicClient.Resource(configMapGVR).Namespace(metav1.NamespaceAll).Watch(context.TODO(), options)
			},
		},
		&unstructured.Unstructured{},
		0,
		cache.Indexers{},
	)

	configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			queue.Add(obj)
		},
	})

	return &Controller{
		dynamicClient:      dynamicClient,
		queue:              queue,
		myResourceInformer: myResourceInformer,
		configMapInformer:  configMapInformer,
	}
}

func (c *Controller) Run(stopCh chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	go c.myResourceInformer.Run(stopCh)
	go c.configMapInformer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.myResourceInformer.HasSynced, c.configMapInformer.HasSynced) {
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
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected Unstructured object but got %T", obj)
	}

	switch unstructuredObj.GetKind() {
	case "ConfigMap":
		return c.recreateConfigMap(unstructuredObj)
	default:
		return c.processMyResource(unstructuredObj)
	}
}

func (c *Controller) processMyResource(obj *unstructured.Unstructured) error {
	fmt.Printf("Processing MyResource: %s/%s\n", obj.GetNamespace(), obj.GetName())

	name, found, err := unstructured.NestedString(obj.Object, "spec", "name")
	if err != nil || !found {
		return fmt.Errorf("failed to retrieve 'name' from MyResource: %v", err)
	}
	configKey, found, err := unstructured.NestedString(obj.Object, "spec", "configKey")
	if err != nil || !found {
		return fmt.Errorf("failed to retrieve 'configKey' from MyResource: %v", err)
	}
	configValue, found, err := unstructured.NestedString(obj.Object, "spec", "configValue")
	if err != nil || !found {
		return fmt.Errorf("failed to retrieve 'configValue' from MyResource: %v", err)
	}

	configMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: obj.GetNamespace(),
		},
		Data: map[string]string{
			configKey: configValue,
		},
	}

	configMapUnstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(configMap)
	if err != nil {
		return fmt.Errorf("failed to convert ConfigMap to Unstructured: %v", err)
	}

	_, err = c.dynamicClient.Resource(configMapGVR).Namespace(obj.GetNamespace()).Create(context.TODO(), &unstructured.Unstructured{Object: configMapUnstructured}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create ConfigMap: %v", err)
	}

	fmt.Printf("ConfigMap %s/%s created successfully.\n", obj.GetNamespace(), name)
	return nil
}

func (c *Controller) recreateConfigMap(obj *unstructured.Unstructured) error {
	name := obj.GetName()
	namespace := obj.GetNamespace()

	fmt.Printf("ConfigMap %s/%s was deleted, attempting to recreate...\n", namespace, name)

	// 対応する MyResource を探す
	myResourceList, err := c.dynamicClient.Resource(myResourceGVR).Namespace(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list MyResources: %v", err)
	}

	for _, myResource := range myResourceList.Items {
		resourceName, found, err := unstructured.NestedString(myResource.Object, "spec", "name")
		if err != nil || !found {
			continue
		}

		if resourceName == name {
			return c.processMyResource(&myResource)
		}
	}

	return fmt.Errorf("no corresponding MyResource found for ConfigMap %s/%s", namespace, name)
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
