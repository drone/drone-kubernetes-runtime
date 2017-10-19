package kubernetes

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/drone/drone-runtime/engine"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

type Engine struct {
	client       *kubernetes.Clientset
	namespace    string
	storageClass string
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

func WithStorageClass(sc string) Option {
	return func(e *Engine) error {
		e.storageClass = sc
		return nil
	}
}

// New returns a new Kubernetes Engine.
func New(opts ...Option) (*Engine, error) {
	e := &Engine{
		namespace:    metav1.NamespaceDefault,
		storageClass: "generic", // TODO: set the default storage class here
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

// Setup creates a PersistentVolumeClaim which will be shared across all pods/containers in the pipeline
func (e *Engine) Setup(ctx context.Context, conf *engine.Config) error {
	// We don't need to create a PVC if no volumes are requested
	if len(conf.Volumes) == 0 {
		return nil
	}

	for _, vol := range conf.Volumes {
		pvc := &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      volumeName(vol.Name),
				Namespace: e.namespace,
			},
			Spec: v1.PersistentVolumeClaimSpec{
				AccessModes:      []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
				StorageClassName: &e.storageClass,
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceStorage: resource.MustParse("1G"),
					},
				},
			},
		}

		_, err := e.client.CoreV1().PersistentVolumeClaims(e.namespace).Create(pvc)
		if err != nil {
			return err
		}
	}

	return nil
}

func (e *Engine) Create(ctx context.Context, proc *engine.Step) error {
	// TODO: Is this actually needed for Kubernetes? Can we do something smart here?
	return nil
}

func (e *Engine) Start(ctx context.Context, proc *engine.Step) error {
	workingDir := proc.WorkingDir
	if proc.Alias == "clone" && len(proc.Volumes) > 0 {
		workingDir = volumeMountPath(proc.Volumes[0].Name)
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dnsName(proc.Name),
			Namespace: e.namespace,
			Labels:    proc.Labels,
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever,
			Containers: []v1.Container{{
				Name:       dnsName(proc.Alias),
				Image:      proc.Image,
				Command:    proc.Entrypoint,
				Args:       proc.Command,
				WorkingDir: workingDir,
				Env:        mapToEnvVars(proc.Environment),
			}},
		},
	}

	if len(proc.Volumes) > 0 {
		var vols []v1.Volume
		var volMounts []v1.VolumeMount

		for _, vol := range proc.Volumes {
			vols = append(vols, v1.Volume{
				Name: volumeName(vol.Name),
				VolumeSource: v1.VolumeSource{
					PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
						ClaimName: volumeName(vol.Name),
						ReadOnly:  false,
					},
				},
			})

			volMounts = append(volMounts, v1.VolumeMount{
				Name:      volumeName(vol.Name),
				MountPath: volumeMountPath(vol.Name),
			})
		}

		pod.Spec.Volumes = vols
		for i := range pod.Spec.Containers {
			pod.Spec.Containers[i].VolumeMounts = volMounts
		}
	}

	_, err := e.client.CoreV1().Pods(e.namespace).Create(pod)
	return err
}

func (e *Engine) Wait(ctx context.Context, proc *engine.Step) (*engine.State, error) {
	finished := make(chan bool)

	var podUpdated = func(old interface{}, new interface{}) {
		pod := new.(*v1.Pod)
		if pod.Name == dnsName(proc.Name) {
			switch pod.Status.Phase {
			case v1.PodSucceeded, v1.PodFailed:
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

	pod, err := e.client.CoreV1().Pods(e.namespace).Get(dnsName(proc.Name), metav1.GetOptions{
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

func (e *Engine) Tail(ctx context.Context, proc *engine.Step) (io.ReadCloser, error) {
	up := make(chan bool)

	var podUpdated = func(old interface{}, new interface{}) {
		pod := new.(*v1.Pod)
		if pod.Name == dnsName(proc.Name) {
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
		Name(dnsName(proc.Name)).
		Resource("pods").
		SubResource("log").
		VersionedParams(opts, scheme.ParameterCodec).
		Stream()

}

func (e *Engine) Upload(ctx context.Context, proc *engine.Step, path string, r io.Reader) error {
	panic("implement me")
}

func (e *Engine) Download(ctx context.Context, proc *engine.Step, path string) (io.ReadCloser, *engine.FileInfo, error) {
	panic("implement me")
}

func (e *Engine) Destroy(ctx context.Context, conf *engine.Config) error {
	var gracePeriodSeconds int64 = 0 // immediately

	dpb := metav1.DeletePropagationBackground

	for _, stage := range conf.Stages {
		for _, step := range stage.Steps {
			opts := &metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriodSeconds,
				PropagationPolicy:  &dpb,
			}

			if err := e.client.CoreV1().Pods(e.namespace).Delete(dnsName(step.Name), opts); err != nil {
				return err
			}
		}
	}

	if len(conf.Volumes) > 0 {
		vol := volumeName(conf.Volumes[0].Name)
		return e.client.CoreV1().PersistentVolumeClaims(e.namespace).Delete(vol, &metav1.DeleteOptions{})
	}

	return nil
}

func mapToEnvVars(m map[string]string) []v1.EnvVar {
	var ev []v1.EnvVar
	for k, v := range m {
		ev = append(ev, v1.EnvVar{
			Name:  k,
			Value: v,
		})
	}
	return ev
}

func dnsName(i string) string {
	return strings.Replace(i, "_", "-", -1)
}

func volumeName(i string) string {
	return dnsName(strings.Split(i, ":")[0])
}

func volumeMountPath(i string) string {
	s := strings.Split(i, ":")
	if len(s) > 1 {
		return s[1]
	}
	return s[0]
}
