package kubernetes

import (
	"context"
	"io"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/drone/drone-runtime/engine"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

type Engine struct {
	client    *kubernetes.Clientset
	namespace string
}

type Option func(e *Engine) error

func WithConfig(masterURL, kubeconfigPath string) Option {
	return func(e *Engine) error {
		config, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfigPath)
		if err != nil {
			return err
		}

		e.client, err = kubernetes.NewForConfig(config)
		return err
	}
}

func WithNamespace(ns string) Option {
	return func(e *Engine) error {
		e.namespace = ns
		return nil
	}
}

// New returns a new Kubernetes Engine.
func New(opts ...Option) (*Engine, error) {
	e := &Engine{
		namespace: metav1.NamespaceDefault,
	}

	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, err
		}
	}

	// If no client was set via an option, set inClusterConfig
	if e.client == nil {
		opt := WithConfig("", "")
		if err := opt(e); err != nil {
			return nil, err
		}
	}

	return e, nil
}

func NewEnv() (*Engine, error) {
	var opts []Option

	masterURL := os.Getenv("DRONE_KUBERNETES_MASTER")
	kubeconfig := os.Getenv("DRONE_KUBERNETES_KUBECONFIG")
	if masterURL != "" || kubeconfig != "" {
		opts = append(opts, WithConfig(masterURL, kubeconfig))
	}

	if ns := os.Getenv("DRONE_KUBERNETES_NAMESPACE"); ns != "" {
		opts = append(opts, WithNamespace(ns))
	}

	return New(opts...)
}

// Setup creates a PersistentVolumeClaim which will be shared across all pods/containers in the pipeline
func (e *Engine) Setup(ctx context.Context, conf *engine.Config) error {
	// We don't need to create a PVC if no volumes are requested
	if len(conf.Volumes) == 0 {
		return nil
	}

	nodeList, err := e.client.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	var nodeNames []string
	for _, n := range nodeList.Items {
		nodeNames = append(nodeNames, n.Name)
	}

	// Pick random node for now.
	node := nodeNames[rand.Intn(len(nodeNames)-1)]

	for _, vol := range conf.Volumes {
		pv := PersistentVolume(node, e.namespace, vol.Name)
		_, err := e.client.CoreV1().PersistentVolumes().Create(pv)
		if err != nil {
			return err
		}

		pvc := PersistentVolumeClaim(e.namespace, vol.Name)
		_, err = e.client.CoreV1().PersistentVolumeClaims(e.namespace).Create(pvc)
		if err != nil {
			return err
		}
	}

	return nil
}

func (e *Engine) Create(ctx context.Context, step *engine.Step) error {
	return nil
}

func (e *Engine) Start(ctx context.Context, step *engine.Step) error {
	pod := Pod(e.namespace, step)

	for _, n := range step.Networks {
		svc := Service(e.namespace, n.Aliases[0], pod.Name, n.Ports)
		if svc == nil {
			continue
		}
		if _, err := e.client.CoreV1().Services(e.namespace).Create(svc); err != nil {
			return err
		}
	}

	_, err := e.client.CoreV1().Pods(e.namespace).Create(pod)
	return err
}

func (e *Engine) Wait(ctx context.Context, step *engine.Step) (*engine.State, error) {
	podName := podName(step)

	finished := make(chan bool)
	var podUpdated = func(old interface{}, new interface{}) {
		pod := new.(*v1.Pod)
		if pod.Name == podName {
			switch pod.Status.Phase {
			case v1.PodSucceeded, v1.PodFailed, v1.PodUnknown:
				finished <- true
			}
		}
	}

	si := informers.NewSharedInformerFactory(e.client, 5*time.Minute)
	si.Core().V1().Pods().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: podUpdated,
		},
	)
	si.Start(wait.NeverStop)

	<-finished

	pod, err := e.client.CoreV1().Pods(e.namespace).Get(podName, metav1.GetOptions{
		IncludeUninitialized: true,
	})
	if err != nil {
		return nil, err
	}

	bs := &engine.State{
		ExitCode:  int(pod.Status.ContainerStatuses[0].State.Terminated.ExitCode),
		Exited:    true,
		OOMKilled: false,
	}

	return bs, nil

}

func (e *Engine) Tail(ctx context.Context, step *engine.Step) (io.ReadCloser, error) {
	podName := podName(step)

	up := make(chan bool)

	var podUpdated = func(old interface{}, new interface{}) {
		pod := new.(*v1.Pod)
		if pod.Name == podName {
			switch pod.Status.Phase {
			case v1.PodRunning, v1.PodSucceeded, v1.PodFailed:
				up <- true
			}
		}
	}

	si := informers.NewSharedInformerFactory(e.client, 5*time.Minute)
	si.Core().V1().Pods().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: podUpdated,
		},
	)
	si.Start(wait.NeverStop)

	<-up

	opts := &v1.PodLogOptions{
		Follow: true,
	}

	return e.client.CoreV1().RESTClient().Get().
		Namespace(e.namespace).
		Name(podName).
		Resource("pods").
		SubResource("log").
		VersionedParams(opts, scheme.ParameterCodec).
		Stream()
}

func (e *Engine) Upload(ctx context.Context, step *engine.Step, path string, r io.Reader) error {
	panic("won't be implemented")
}

func (e *Engine) Download(ctx context.Context, step *engine.Step, path string) (io.ReadCloser, *engine.FileInfo, error) {
	panic("won't be implemented")
}

func (e *Engine) Destroy(ctx context.Context, conf *engine.Config) error {
	var gracePeriodSeconds int64 = 0 // immediately
	dpb := metav1.DeletePropagationBackground

	deleteOpts := &metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriodSeconds,
		PropagationPolicy:  &dpb,
	}

	for _, stage := range conf.Stages {
		for _, step := range stage.Steps {
			if err := e.client.CoreV1().Pods(e.namespace).Delete(podName(step), deleteOpts); err != nil {
				return err
			}

			for _, n := range step.Networks {
				svc := Service(e.namespace, n.Aliases[0], step.Alias, n.Ports)
				if svc == nil {
					continue
				}
				if err := e.client.CoreV1().Services(e.namespace).Delete(svc.Name, deleteOpts); err != nil {
					return err
				}
			}
		}
	}

	for _, vol := range conf.Volumes {
		pvc := PersistentVolumeClaim(e.namespace, vol.Name)
		err := e.client.CoreV1().PersistentVolumeClaims(e.namespace).Delete(pvc.Name, deleteOpts)
		if err != nil {
			return err
		}

		pv := PersistentVolume("", "", vol.Name)
		err = e.client.CoreV1().PersistentVolumes().Delete(pv.Name, deleteOpts)
		if err != nil {
			return err
		}
	}

	return nil
}

func dnsName(i string) string {
	return strings.Replace(i, "_", "-", -1)
}
