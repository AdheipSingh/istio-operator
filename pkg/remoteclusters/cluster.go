/*
Copyright 2019 Banzai Cloud.

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

package remoteclusters

import (
	"context"
	"sync"

	"github.com/go-logr/logr"
	"github.com/goph/emperror"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	istiov1beta1 "github.com/banzaicloud/istio-operator/pkg/apis/istio/v1beta1"
	"github.com/banzaicloud/istio-operator/pkg/controller/meshgateway"
)

type Cluster struct {
	name   string
	config []byte
	log    logr.Logger

	stop          <-chan struct{}
	stopper       chan<- struct{}
	initClient    sync.Once
	initInformers sync.Once

	mgr manager.Manager

	restConfig        *rest.Config
	ctrlRuntimeClient client.Client
	dynamicClient     dynamic.Interface
	istioConfig       *istiov1beta1.Istio
	remoteConfig      *istiov1beta1.RemoteIstio
	ctrl              controller.Controller
}

func NewCluster(name string, ctrl controller.Controller, config []byte, log logr.Logger) (*Cluster, error) {
	stop := make(chan struct{})

	cluster := &Cluster{
		name:    name,
		config:  config,
		log:     log.WithValues("cluster", name),
		stop:    stop,
		stopper: stop,
		ctrl:    ctrl,
	}

	restConfig, err := cluster.getRestConfig(config)
	if err != nil {
		return nil, emperror.Wrap(err, "could not get k8s rest config")
	}
	cluster.restConfig = restConfig

	return cluster, nil
}

func (c *Cluster) GetName() string {
	return c.name
}

func (c *Cluster) initK8sInformers() error {
	if c.remoteConfig == nil {
		return errors.New("remoteconfig must be set")
	}

	informer, err := c.mgr.GetCache().GetInformerForKind(corev1.SchemeGroupVersion.WithKind("Namespace"))
	if err != nil {
		return emperror.Wrap(err, "could not get informer for namespaces")
	}

	err = c.ctrl.Watch(&source.Informer{
		Informer: informer,
	}, &handler.EnqueueRequestsFromMapFunc{
		ToRequests: handler.ToRequestsFunc(func(object handler.MapObject) []reconcile.Request {
			return []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{
						Name:      c.remoteConfig.Name,
						Namespace: c.remoteConfig.Namespace,
					},
				},
			}
		}),
	}, predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return false
		},
	})

	if err != nil {
		return emperror.Wrap(err, "could not set watcher for namespaces informer")
	}

	return nil
}

func (c *Cluster) initK8SClients() error {
	err := c.startManager(c.restConfig)
	if err != nil {
		return err
	}

	// add mesh gateway controller to the manager
	meshgateway.Add(c.mgr)

	c.ctrlRuntimeClient = c.mgr.GetClient()

	dynamicClient, err := dynamic.NewForConfig(c.restConfig)
	if err != nil {
		return emperror.Wrap(err, "could not get dynamic client")
	}
	c.dynamicClient = dynamicClient

	return nil
}

func (c *Cluster) Reconcile(remoteConfig *istiov1beta1.RemoteIstio, istio *istiov1beta1.Istio) error {
	c.log.Info("reconciling remote istio")

	var ReconcilerFuncs []func(remoteConfig *istiov1beta1.RemoteIstio, istio *istiov1beta1.Istio) error

	err := c.reconcileCRDs(remoteConfig, istio)
	if err != nil {
		return emperror.Wrapf(err, "could not reconcile")
	}

	c.remoteConfig = remoteConfig

	// init k8s clients
	c.initClient.Do(func() {
		err = c.initK8SClients()
	})
	if err != nil {
		return emperror.Wrap(err, "could not init k8s clients")
	}

	// init k8s informers
	c.initInformers.Do(func() {
		err = c.initK8sInformers()
	})
	if err != nil {
		return emperror.Wrap(err, "could not init k8s informers")
	}

	ReconcilerFuncs = append(ReconcilerFuncs,
		c.reconcileConfig,
		c.reconcileSignCert,
		c.reconcileCARootToNamespaces,
		c.reconcileEnabledServices,
		c.ReconcileEnabledServiceEndpoints,
		c.reconcileComponents,
	)

	for _, f := range ReconcilerFuncs {
		if err := f(remoteConfig, istio); err != nil {
			return emperror.Wrapf(err, "could not reconcile")
		}
	}

	return nil
}

func (c *Cluster) GetRemoteConfig() *istiov1beta1.RemoteIstio {
	return c.remoteConfig
}

func (c *Cluster) RemoveRemoteIstioComponents() error {
	if c.istioConfig == nil {
		return nil
	}
	c.log.Info("removing istio from remote cluster by removing installed CRDs")

	err := c.ctrlRuntimeClient.Delete(context.TODO(), c.istiocrd())
	if err != nil && !k8serrors.IsNotFound(err) {
		emperror.Wrap(err, "could not remove istio CRD from remote cluster")
	}

	err = c.ctrlRuntimeClient.Delete(context.TODO(), c.meshgatewaycrd())
	if err != nil && k8serrors.IsNotFound(err) {
		return emperror.Wrap(err, "could not remove mesh gateway CRD from remote cluster")
	}

	return nil
}

func (c *Cluster) Shutdown() {
	c.log.Info("shutdown remote cluster manager")
	close(c.stopper)
}

func (c *Cluster) getRestConfig(kubeconfig []byte) (*rest.Config, error) {
	clusterConfig, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return nil, emperror.Wrap(err, "could not load kubeconfig")
	}

	rest, err := clientcmd.NewDefaultClientConfig(*clusterConfig, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, emperror.Wrap(err, "could not create k8s rest config")
	}

	return rest, nil
}

func (c *Cluster) startManager(config *rest.Config) error {
	mgr, err := manager.New(config, manager.Options{
		MetricsBindAddress: "0", // disable metrics
	})
	if err != nil {
		return emperror.Wrap(err, "could not create manager")
	}

	c.mgr = mgr
	go func() {
		c.mgr.Start(c.stop)
	}()

	c.mgr.GetCache().WaitForCacheSync(c.stop)

	return nil
}
