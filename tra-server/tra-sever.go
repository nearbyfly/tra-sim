package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"time"

	"net/http"
	"io/ioutil"
	"path/filepath"
	"encoding/json"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"

	"k8s.io/client-go/util/homedir"

	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
	pb "github.com/nearbyfly/tra-sim/tra"
)

var (
	data = map[string]*pb.TraResponse{}
)

// DeltaType is the type of a change (addition, deletion, etc)
type DeltaType string

// Change type definition
const (
	Added   DeltaType = "Added"
	Updated DeltaType = "Updated"
	Deleted DeltaType = "Deleted"
)

const (
	port = ":50053"
)

var (
	streamCache = make(map[string] map[pb.TraService_SubscribeServer]struct{})
)

type QueueItem struct {
	Type DeltaType
	Obj  interface{}
}

type watchInterface interface {
	add(obj interface{})
	modify(obj interface{})
	delete(obj interface{})
}

type endpointAction struct {
}

func (s *endpointAction) add(obj interface{}) {
	endpoints, ok := obj.(*v1.Endpoints)
	if !ok {
		panic("Could not cast to Endpoint")
	}

	fqdn := endpoints.ObjectMeta.Name + "." + endpoints.Namespace + ".svc.cluster.local"
	data[fqdn] = &pb.TraResponse{
		Fqdn: fqdn,
	}

	for _, address := range endpoints.Subsets[0].Addresses {
		data[fqdn].Nodes = append(data[fqdn].Nodes, &pb.Node{NodeId: address.IP, Ip: address.IP, SipPort: uint32(endpoints.Subsets[0].Ports[0].Port), Weight: 1})
	}
}

func (s *endpointAction) modify(obj interface{}) {
	endpoints, ok := obj.(*v1.Endpoints)
	if !ok {
		panic("Could not cast to Endpoint")
	}

	fqdn := endpoints.ObjectMeta.Name + "." + endpoints.Namespace + ".svc.cluster.local"
	data[fqdn] = &pb.TraResponse{
		Fqdn: fqdn,
	}

	for _, address := range endpoints.Subsets[0].Addresses {
		data[fqdn].Nodes = append(data[fqdn].Nodes, &pb.Node{NodeId: address.IP, Ip: address.IP, SipPort: uint32(endpoints.Subsets[0].Ports[0].Port), Weight: 20})
	}
}

func (s *endpointAction) delete(obj interface{}) {
	endpoints, ok := obj.(*v1.Endpoints)
	if !ok {
		panic("Could not cast to Endpoint")
	}

	fqdn := endpoints.ObjectMeta.Name + "." + endpoints.Namespace + ".svc.cluster.local"
	delete(data, fqdn)
}

// Controller demonstrates how to implement a controller with client-go.
type Controller struct {
	indexer  cache.Indexer
	queue    workqueue.RateLimitingInterface
	informer cache.Controller
	endpoint_action endpointAction
}

// NewController creates a new Controller.
func NewController(queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller) *Controller {
	return &Controller{
		informer: informer,
		indexer:  indexer,
		queue:    queue,
	}
}

func (c *Controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	e, quit := c.queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.queue.Done(e)

	// Invoke the method containing the business logic
	err := c.do(e.(QueueItem).Type, e.(QueueItem).Obj.(string))
	// Handle the error if something went wrong during the execution of the business logic
	c.handleErr(err, e)
	return true
}

// syncToStdout is the business logic of the controller. In this controller it simply prints
// information about the pod to stdout. In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
func (c *Controller) do(t DeltaType, k string) error {
	obj, exists, err := c.indexer.GetByKey(k)
	if err != nil {
		klog.Errorf("Fetching object with key %s from store failed with %v", k, err)
		return err
	}

	if !exists {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		klog.Infof("Endpoint %s does not exist anymore\n", k)
	} else {
		// Note that you also have to check the uid if you have a local controlled resource, which
		// is dependent on the actual instance, to detect that a Pod was recreated with the same name
		klog.Infof("Sync/Add/Update for Endpoint %v %s\n", t, obj.(*v1.Endpoints).GetName())

		switch t {
		case Added:
			c.endpoint_action.add(obj)
		case Updated:
			c.endpoint_action.modify(obj)
		case Deleted:
			c.endpoint_action.delete(obj)
		}
	}
	return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		klog.Infof("Error syncing pod %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	klog.Infof("Dropping pod %q out of the queue: %v", key, err)
}

// Run begins watching and syncing.
func (c *Controller) Run(threadiness int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	// Let the workers stop when we are done
	defer c.queue.ShutDown()
	klog.Info("Starting Endpoints controller")

	go c.informer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	klog.Info("Stopping Endpoints controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func watch() {
	var kubeconfig *string
	var master string

	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	flag.StringVar(&master, "master", "", "master url")

	flag.Parse()

	// creates the connection
	config, err := clientcmd.BuildConfigFromFlags(master, *kubeconfig)
	if err != nil {
		klog.Fatal(err)
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	// create the endpoint watcher
	endpointWatcher := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "endpoints", v1.NamespaceDefault, fields.Everything())

	// create the workqueue
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	// Bind the workqueue to a cache with the help of an informer. This way we make sure that
	// whenever the cache is updated, the pod key is added to the workqueue.
	// Note that when we finally process the item from the workqueue, we might see a newer version
	// of the Pod than the version which was responsible for triggering the update.
	indexer, informer := cache.NewIndexerInformer(endpointWatcher, &v1.Endpoints{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(QueueItem{Type: Added, Obj: key})
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(QueueItem{Type: Updated, Obj: key})
			}
		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(QueueItem{Type: Deleted, Obj: key})
			}
		},
	}, cache.Indexers{})

	controller := NewController(queue, indexer, informer)

	// We can now warm up the cache for initial synchronization.
	// Let's suppose that we knew about a pod "mypod" on our last run, therefore add it to the cache.
	// If this pod is not there anymore, the controller will be notified about the removal after the
	// cache has synchronized.
	// indexer.Add(&v1.Pod{
	// 	ObjectMeta: meta_v1.ObjectMeta{
	// 		Name:      "mypod",
	// 		Namespace: v1.NamespaceDefault,
	// 	},
	// })

	// Now let's start the controller
	stop := make(chan struct{})
	defer close(stop)
	controller.Run(1, stop)
}

type NodeData struct {
	Fqdn     string `json:"fqdn"`
	Node_id  string `json:"node_id"`
	Ip       string `json:"ip"`
	Sip_port uint32 `json:"sip_port"`
	Weight   uint32 `json:"weight"`
}

// server is used to implement helloworld.GreeterServer.
type server struct {
	pb.UnimplementedTraServiceServer
}

// SayHello implements helloworld.GreeterServer
func (s *server) Nodes(ctx context.Context, in *pb.TraRequest) (*pb.TraResponse, error) {
	p, _ := peer.FromContext(ctx)
	klog.Infof("GRPC Nodes Received from %s : %v", p.Addr.String(), in.Fqdn)
	var response = data[in.Fqdn]
	klog.Infoln(response)
	return response, nil
	//	return &pb.TraResponse{Fqdn: in.Fqdn, Nodes: []*pb.Node{
	//			&pb.Node{NodeId: "1", Ip: "192.168.0.1", SipPort: 5060, Weight: 50},
	//			&pb.Node{NodeId: "2", Ip: "192.168.0.2", SipPort: 5060, Weight: 50},
	//	}}, nil
}

func (s *server) Subscribe(in *pb.TraRequest, stream pb.TraService_SubscribeServer) error {
	p, _ := peer.FromContext(stream.Context())
	klog.Infof("GRPC Subscribe Recieved from %s : %v", p.Addr.String(), in.Fqdn)

	if _, ok := streamCache[in.Fqdn]; !ok {
		memember := make(map[pb.TraService_SubscribeServer] struct{})
		memember[stream] = struct{}{}
		streamCache[in.Fqdn] = memember
	} else {
		streamCache[in.Fqdn][stream] = struct{}{}
	}

	change_chan <- struct{}{}
	for {
		if err := stream.Context().Err(); err != nil {
			delete(streamCache[in.Fqdn], stream)
			break
		}
		time.Sleep(time.Second)
	}
	return nil
}

var change_chan = make(chan struct{})

func (s *server) Notify() error {
	for {
		_ = <-change_chan
		klog.Infof("GRPC Notify %+v\n", streamCache)
		for k, stream_list := range streamCache {
			for stream, _ := range stream_list {
				if err := stream.Context().Err(); err != nil {
					delete(streamCache[k], stream)
					continue
				}
				stream.Send(data[k])

				p, _ := peer.FromContext(stream.Context())
				klog.Infof("     send to %+v\n", p.Addr.String())
			}
		}
	}
}

func grpcServer() {
	lis, err := net.Listen("tcp", port)
	if err != nil {
		klog.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	myserver := server{}
	go myserver.Notify()

	pb.RegisterTraServiceServer(s, &myserver)
	if err := s.Serve(lis); err != nil {
		klog.Fatalf("failed to serve: %v", err)
	}
}


func updateNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			klog.Infoln("Read failed:", err)
		}
		defer r.Body.Close()

		var update_data []NodeData
		err = json.Unmarshal(b, &update_data)
		if err != nil {
			klog.Infoln("json format error:", err)
		}
		klog.Infof("%s update_data: %#v", r.Method, update_data)

		data = make(map[string]*pb.TraResponse)
		for _, v := range update_data {
			if _, ok := data[v.Fqdn]; ok {
				data[v.Fqdn].Nodes = append(data[v.Fqdn].Nodes, &pb.Node{NodeId: v.Node_id, Ip: v.Ip, SipPort: v.Sip_port, Weight: v.Weight})
			} else {
				data[v.Fqdn] = &pb.TraResponse{
					Fqdn: v.Fqdn,
					Nodes: []*pb.Node{
						&pb.Node{NodeId: v.Node_id, Ip: v.Ip, SipPort: v.Sip_port, Weight: v.Weight},
					},
				}
			}
		}

		for k, v := range data {
			klog.Infoln(k, v.Nodes)
		}

		change_chan <- struct{}{}

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		return
	} else if r.Method == "GET" {
		var tmp []NodeData
		for _, v := range data {
			for _, n := range v.Nodes {
				tmp = append(tmp, NodeData{v.Fqdn, n.NodeId, n.Ip, n.SipPort, n.Weight})
			}
		}

		b, err := json.Marshal(tmp)
		if err != nil {
			klog.Infoln("json format error:", err)
			return
		}

		klog.Infof("%s get_data: %#v", r.Method, tmp)

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Write(b)
	} else {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		return
	}
}

func httpServer() {
	http.HandleFunc("/update", updateNodes)
	http.ListenAndServe(":50052", nil)
}


func main() {
	go watch()
	go grpcServer()
	go httpServer()

	// Wait forever
	select {}
}
