package k8s // import "go.universe.tf/metallb/internal/k8s"

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-kit/kit/log"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

// Client watches a Kubernetes cluster and translates events into
// Controller method calls.
type Client struct {
	logger log.Logger

	client *kubernetes.Clientset
	events record.EventRecorder
	queue  workqueue.RateLimitingInterface

	svcIndexer   cache.Indexer
	svcInformer  cache.Controller

	syncFuncs []cache.InformerSynced

	serviceChanged func(log.Logger, string, *v1.Service, *v1.Endpoints) SyncState
}

// SyncState is the result of calling synchronization callbacks.
type SyncState int

const (
	// The update was processed successfully.
	SyncStateSuccess SyncState = iota
	// The update caused a transient error, the k8s client should
	// retry later.
	SyncStateError
	// The update was accepted, but requires reprocessing all watched
	// services.
	SyncStateReprocessAll
)

// Config specifies the configuration of the Kubernetes
// client/watcher.
type Config struct {
	ProcessName   string
	ConfigMapName string
	NodeName      string
	MetricsHost   string
	MetricsPort   int
	Logger        log.Logger
	Kubeconfig    string

	ServiceChanged func(log.Logger, string, *v1.Service, *v1.Endpoints) SyncState
}

type svcKey string

// New connects to masterAddr, using kubeconfig to authenticate.
//
// The client uses processName to identify itself to the cluster
// (e.g. when logging events).
func New(cfg *Config) (*Client, error) {
	var (
		k8sConfig *rest.Config
		err error
	)

	if cfg.Kubeconfig == "" {
		// if the user didn't provide a config file, assume that we're
		// running inside k8s
		k8sConfig, err = rest.InClusterConfig()
	} else {
		// the user provided a config file, so use that
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("building client config: %s", err)
	}
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return nil, fmt.Errorf("creating Kubernetes client: %s", err)
	}

	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(clientset.CoreV1().RESTClient()).Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: cfg.ProcessName})

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	c := &Client{
		logger: cfg.Logger,
		client: clientset,
		events: recorder,
		queue:  queue,
	}

	if cfg.ServiceChanged != nil {
		svcHandlers := cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					c.queue.Add(svcKey(key))
				}
			},
			UpdateFunc: func(old interface{}, new interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(new)
				if err == nil {
					c.queue.Add(svcKey(key))
				}
			},
			DeleteFunc: func(obj interface{}) {
				key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
				if err == nil {
					c.queue.Add(svcKey(key))
				}
			},
		}
		svcWatcher := cache.NewListWatchFromClient(c.client.CoreV1().RESTClient(), "services", v1.NamespaceAll, fields.Everything())
		c.svcIndexer, c.svcInformer = cache.NewIndexerInformer(svcWatcher, &v1.Service{}, 0, svcHandlers, cache.Indexers{})

		c.serviceChanged = cfg.ServiceChanged
		c.syncFuncs = append(c.syncFuncs, c.svcInformer.HasSynced)
	}

	http.Handle("/metrics", promhttp.Handler())
	go func() {
		http.ListenAndServe(fmt.Sprintf("%s:%d", cfg.MetricsHost, cfg.MetricsPort), nil)
	}()

	return c, nil
}

// Run watches for events on the Kubernetes cluster, and dispatches
// calls to the Controller.
func (c *Client) Run(stopCh <-chan struct{}) error {
	if c.svcInformer != nil {
		go c.svcInformer.Run(stopCh)
	}

	if !cache.WaitForCacheSync(stopCh, c.syncFuncs...) {
		return errors.New("timed out waiting for cache sync")
	}

	if stopCh != nil {
		go func() {
			<-stopCh
			c.queue.ShutDown()
		}()
	}

	for {
		key, quit := c.queue.Get()
		if quit {
			return nil
		}
		updates.Inc()
		st := c.sync(key)
		switch st {
		case SyncStateSuccess:
			c.queue.Forget(key)
		case SyncStateError:
			updateErrors.Inc()
			c.queue.AddRateLimited(key)
		case SyncStateReprocessAll:
			c.queue.Forget(key)
			c.ForceSync()
		}
	}
}

// ForceSync reprocess all watched services
func (c *Client) ForceSync() {
	if c.svcIndexer != nil {
		for _, k := range c.svcIndexer.ListKeys() {
			c.queue.AddRateLimited(svcKey(k))
		}
	}
}

// UpdateStatus writes the protected "status" field of svc back into
// the Kubernetes cluster.
func (c *Client) UpdateStatus(svc *v1.Service) error {
	_, err := c.client.CoreV1().Services(svc.Namespace).UpdateStatus(svc)
	return err
}

// Infof logs an informational event about svc to the Kubernetes cluster.
func (c *Client) Infof(svc *v1.Service, kind, msg string, args ...interface{}) {
	c.events.Eventf(svc, v1.EventTypeNormal, kind, msg, args...)
}

// Errorf logs an error event about svc to the Kubernetes cluster.
func (c *Client) Errorf(svc *v1.Service, kind, msg string, args ...interface{}) {
	c.events.Eventf(svc, v1.EventTypeWarning, kind, msg, args...)
}

func (c *Client) sync(key interface{}) SyncState {
	defer c.queue.Done(key)

	switch k := key.(type) {
	case svcKey:
		l := log.With(c.logger, "service", string(k))
		svc, exists, err := c.svcIndexer.GetByKey(string(k))
		if err != nil {
			l.Log("op", "getService", "error", err, "msg", "failed to get service")
			return SyncStateError
		}
		if !exists {
			return c.serviceChanged(l, string(k), nil, nil)
		}
		return c.serviceChanged(l, string(k), svc.(*v1.Service), nil)

	default:
		panic(fmt.Errorf("unknown key type for %#v (%T)", key, key))
	}
}
