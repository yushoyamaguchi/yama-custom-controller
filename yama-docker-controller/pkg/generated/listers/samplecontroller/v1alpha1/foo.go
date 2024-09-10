/*
Copyright The Kubernetes Authors.

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

// Code generated by lister-gen. DO NOT EDIT.

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/listers"
	"k8s.io/client-go/tools/cache"
	v1alpha1 "k8s.io/sample-controller/pkg/apis/samplecontroller/v1alpha1"
)

// YamaDockerLister helps list Foos.
// All objects returned here must be treated as read-only.
type YamaDockerLister interface {
	// List lists all Foos in the indexer.
	// Objects returned here must be treated as read-only.
	List(selector labels.Selector) (ret []*v1alpha1.Foo, err error)
	// Foos returns an object that can list and get Foos.
	Foos(namespace string) YamaDockerNamespaceLister
	YamaDockerListerExpansion
}

// yamaDockerLister implements the YamaDockerLister interface.
type yamaDockerLister struct {
	listers.ResourceIndexer[*v1alpha1.Foo]
}

// NewYamaDockerLister returns a new YamaDockerLister.
func NewYamaDockerLister(indexer cache.Indexer) YamaDockerLister {
	return &yamaDockerLister{listers.New[*v1alpha1.Foo](indexer, v1alpha1.Resource("foo"))}
}

// Foos returns an object that can list and get Foos.
func (s *yamaDockerLister) Foos(namespace string) YamaDockerNamespaceLister {
	return yamaDockerNamespaceLister{listers.NewNamespaced[*v1alpha1.Foo](s.ResourceIndexer, namespace)}
}

// YamaDockerNamespaceLister helps list and get Foos.
// All objects returned here must be treated as read-only.
type YamaDockerNamespaceLister interface {
	// List lists all Foos in the indexer for a given namespace.
	// Objects returned here must be treated as read-only.
	List(selector labels.Selector) (ret []*v1alpha1.Foo, err error)
	// Get retrieves the Foo from the indexer for a given namespace and name.
	// Objects returned here must be treated as read-only.
	Get(name string) (*v1alpha1.Foo, error)
	YamaDockerNamespaceListerExpansion
}

// fooNamespaceLister implements the FooNamespaceLister
// interface.
type yamaDockerNamespaceLister struct {
	listers.ResourceIndexer[*v1alpha1.Foo]
}
