package kubernetes

import (
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Service(namespace, name, podName string) *v1.Service {
	//// We don't need a service, if we don't have ports
	//if len(n.Ports) == 0 {
	//	continue
	//}
	//
	//var ports []v1.ServicePort
	//for _, p := range n.Ports {
	//	ports = append(ports, v1.ServicePort{
	//		Name:       dnsName(n.Aliases[0]),
	//		Port:       int32(p),
	//		TargetPort: intstr.IntOrString{IntVal: int32(p)},
	//	})
	//}

	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dnsName(name),
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeClusterIP,
			Selector: map[string]string{
				"step": podName,
			},
			//Ports: ports,
		},
	}
}
