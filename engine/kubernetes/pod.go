package kubernetes

import (
	"strings"

	"github.com/drone/drone-runtime/engine"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Pod(namespace string, step *engine.Step) *v1.Pod {

	// TODO: Move volumes stuff to volumes.go?
	var vols []v1.Volume
	var volMounts []v1.VolumeMount
	for _, vol := range step.Volumes {
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
			MountPath: volumeMountPath(step.WorkingDir),
			//MountPath: volumeMountPath(vol.Target),
		})
	}

	pullPolicy := v1.PullIfNotPresent
	if step.Pull {
		pullPolicy = v1.PullAlways
	}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName(step),
			Namespace: namespace,
			Labels: map[string]string{
				"step": podName(step),
			},
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever,
			Containers: []v1.Container{{
				Name:            podName(step),
				Image:           step.Image,
				ImagePullPolicy: pullPolicy,
				Command:         step.Entrypoint,
				Args:            step.Command,
				WorkingDir:      step.WorkingDir,
				Env:             mapToEnvVars(step.Environment),
				VolumeMounts:    volMounts,
			}},
			Volumes: vols,
		},
	}
}

func podName(s *engine.Step) string {
	return dnsName(s.Name)
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

func volumeMountPath(i string) string {
	s := strings.Split(i, ":")
	if len(s) > 1 {
		return s[1]
	}
	return s[0]
}
