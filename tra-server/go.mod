module tra-server

go 1.16

replace github.com/nearbyfly/tra-sim/tra v0.0.0 => ../tra

require (
        github.com/nearbyfly/tra-sim/tra v0.0.0
	google.golang.org/grpc v1.40.0 // indirect
	k8s.io/api v0.22.2 // indirect
	k8s.io/apimachinery v0.22.2 // indirect
	k8s.io/client-go v0.22.2 // indirect
	k8s.io/klog/v2 v2.9.0 // indirect
)
