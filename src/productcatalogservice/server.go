// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	//"os/signal"
	"strings"
	"sync"
	//"syscall"
	"time"

	pb "github.com/GoogleCloudPlatform/microservices-demo/src/productcatalogservice/genproto"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	// "go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"

	// "cloud.google.com/go/profiler"
	"github.com/golang/protobuf/jsonpb"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	cat          pb.ListProductsResponse
	catalogMutex *sync.Mutex
	log          *logrus.Logger
	extraLatency time.Duration

	port = "3550"

	reloadCatalog bool
)

// Initializes an OTLP exporter, and configures the corresponding trace and providers.
func initProvider() func() {
	endpoint := os.Getenv("COLLECTOR_ADDR")
	serviceName := os.Getenv("SERVICE_NAME")

	ctx := context.Background()

	res, err := resource.New(ctx,
		resource.WithAttributes(
			// the service name used to display traces in backends
			semconv.ServiceNameKey.String(serviceName),
		),
	)

	if err != nil {
		log.Fatalf("Failed to create resource: %v", err)

	}

	// Set up a trace exporter
	traceExporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithDialOption(grpc.WithBlock()),
	)

	if err != nil {
		log.Fatalf("Failed to create trace exporter: %v", err)
	}

	// Register the trace exporter with a TracerProvider, using a batch
	// span processor to aggregate spans before export.
	bsp := sdktrace.NewBatchSpanProcessor(traceExporter)
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)
	otel.SetTracerProvider(tracerProvider)

	// set global propagator to tracecontext (the default is no-op).
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return func() {
		// Shutdown will flush any remaining spans and shut down the exporter.
		if tracerProvider.Shutdown(ctx) != nil {
			log.Fatalf("Failed to shutdown TracerProvider: %v", tracerProvider.Shutdown(ctx))
		}
	}
}


var (
	// Add attribute of pod name and node name
	tracer = otel.Tracer("product-tracer")
	podName = os.Getenv("POD_NAME")
	nodeName = os.Getenv("NODE_NAME")

	// labels represent additional key-value descriptors that can be bound to a
	// metric observer or recorder.
	commonLabels = []attribute.KeyValue{
		attribute.String("PodName", podName),
		attribute.String("NodeName", nodeName)}
)

func main() {
	log = logrus.New()
	log.Formatter = &logrus.JSONFormatter{
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "timestamp",
			logrus.FieldKeyLevel: "severity",
			logrus.FieldKeyMsg:   "message",
		},
		TimestampFormat: time.RFC3339Nano,
	}
	log.Out = os.Stdout

	shutdown := initProvider()
	defer shutdown()

	catalogMutex = &sync.Mutex{}
	err := initCatalogFile(&cat)
	if err != nil {
		log.Warnf("could not parse product catalog")
	}

	tracer = otel.Tracer("product-tracer")

	flag.Parse()

	// set injected latency
	if s := os.Getenv("EXTRA_LATENCY"); s != "" {
		v, err := time.ParseDuration(s)
		if err != nil {
			log.Fatalf("failed to parse EXTRA_LATENCY (%s) as time.Duration: %+v", v, err)
		}
		extraLatency = v
		log.Infof("extra latency enabled (duration: %v)", extraLatency)
	} else {
		extraLatency = time.Duration(0)
	}

	// sigs := make(chan os.Signal, 1)
	// signal.Notify(sigs, syscall.SIGUSR1, syscall.SIGUSR2)
	// go func() {
	// 	for {
	// 		sig := <-sigs
	// 		log.Printf("Received signal: %s", sig)
	// 		if sig == syscall.SIGUSR1 {
	// 			reloadCatalog = true
	// 			log.Infof("Enable catalog reloading")
	// 		} else {
	// 			reloadCatalog = false
	// 			log.Infof("Disable catalog reloading")
	// 		}
	// 	}
	// }()
	reloadCatalog = true

	if os.Getenv("PORT") != "" {
		port = os.Getenv("PORT")
	}
	log.Infof("starting grpc server at :%s", port)
	run(port)
	select {}
}

func run(port string) string {
	l, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		log.Fatal(err)
	}
	var srv *grpc.Server

	srv = grpc.NewServer(
		grpc.UnaryInterceptor(otelgrpc.UnaryServerInterceptor()),
		grpc.StreamInterceptor(otelgrpc.StreamServerInterceptor()),
	)

	svc := &productCatalog{}

	pb.RegisterProductCatalogServiceServer(srv, svc)
	healthpb.RegisterHealthServer(srv, svc)
	go srv.Serve(l)
	return l.Addr().String()
}

type productCatalog struct{}

func readCatalogFile(ctx context.Context, catalog *pb.ListProductsResponse) error {
	ctx, readCatalogSpan := tracer.Start(ctx, "hipstershop.ProductCatalogService/ReadCatalog",trace.WithAttributes(commonLabels...))
	defer readCatalogSpan.End()

	catalogMutex.Lock()
	defer catalogMutex.Unlock()

	traceId := readCatalogSpan.SpanContext().TraceID()
	spanId := readCatalogSpan.SpanContext().SpanID()

	log.Infof("TraceID: %v SpanID: %v Start parse product catalog json" , traceId, spanId)

	catalogJSON, err := ioutil.ReadFile("products.json")
	if err != nil {
		log.Fatalf("TraceID: %v SpanID: %v Failed to open product catalog json file: %v", traceId, spanId , err)
		return err
	}
	if err := jsonpb.Unmarshal(bytes.NewReader(catalogJSON), catalog); err != nil {
		log.Fatalf("TraceID: %v SpanID: %v Failed to parse the catalog JSON: %v", traceId, spanId , err)
		return err
	}
	log.Infof("TraceID: %v SpanID: %v Parse product catalog json successfully" , traceId, spanId)
	return nil
}

func initCatalogFile(catalog *pb.ListProductsResponse) error {
	catalogMutex.Lock()
	defer catalogMutex.Unlock()

	catalogJSON, err := ioutil.ReadFile("products.json")
	if err != nil {
		log.Fatalf("Failed to open product catalog json file: %v",err)
		return err
	}
	if err := jsonpb.Unmarshal(bytes.NewReader(catalogJSON), catalog); err != nil {
		log.Fatalf("Failed to parse the catalog JSON: %v",err)
		return err
	}
	log.Infof("Parse product catalog json successfully")
	return nil
}


func parseCatalog(ctx context.Context) []*pb.Product {
	ctx, parseCatalogSpan := tracer.Start(ctx, "hipstershop.ProductCatalogService/ParseCatalog", trace.WithAttributes(commonLabels...))
	defer parseCatalogSpan.End()

	traceId := parseCatalogSpan.SpanContext().TraceID()
	spanId := parseCatalogSpan.SpanContext().SpanID()

	log.Infof("TraceID: %v SpanID: %v Start parse catalog", traceId, spanId)

	if reloadCatalog || len(cat.Products) == 0 {
		err := readCatalogFile(ctx, &cat)
		if err != nil {
			return []*pb.Product{}
			log.Fatalf("TraceID: %v SpanID: %v Parse catalog failed", traceId, spanId)
		}
	}

	log.Infof("TraceID: %v SpanID: %v Parse catalog successfully", traceId, spanId)
	return cat.Products
}

func (p *productCatalog) Check(ctx context.Context, req *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

func (p *productCatalog) Watch(req *healthpb.HealthCheckRequest, ws healthpb.Health_WatchServer) error {
	return status.Errorf(codes.Unimplemented, "health check via Watch not implemented")
}

func (p *productCatalog) ListProducts(ctx context.Context, req *pb.Empty) (*pb.ListProductsResponse, error) {
	time.Sleep(extraLatency)
	// ctx := context.Background()

    listProductSpan := trace.SpanFromContext(ctx)
	listProductSpan.SetAttributes(commonLabels...)

	traceId := listProductSpan.SpanContext().TraceID()
	spanId := listProductSpan.SpanContext().SpanID()

	log.Infof("TraceID: %v SpanID: %v Start list products", traceId, spanId)
	
	listResult := &pb.ListProductsResponse{Products: parseCatalog(ctx)}

	log.Infof("TraceID: %v SpanID: %v List products end", traceId, spanId)

	return listResult, nil
}

func (p *productCatalog) GetProduct(ctx context.Context, req *pb.GetProductRequest) (*pb.Product, error) {
	time.Sleep(extraLatency)
	
	getProductSpan := trace.SpanFromContext(ctx)
	getProductSpan.SetAttributes(commonLabels...)
    traceId := getProductSpan.SpanContext().TraceID()
	spanId := getProductSpan.SpanContext().SpanID()

	log.Infof("TraceID: %v SpanID: %v Search product with name and description %v", traceId, spanId, req.Id)

	var found *pb.Product
	parseCatalogResult := parseCatalog(ctx) 

	for i := 0; i < len(parseCatalogResult); i++ {
		if req.Id == parseCatalogResult[i].Id {
			found = parseCatalogResult[i]
			log.Infof("TraceID: %v SpanID: %v Find product %v", traceId, spanId, found)
		}
	}

	if found == nil {
		log.Fatalf("TraceID: %v SpanID: %v No product with ID %v", traceId, spanId, req.Id)
		return nil, status.Errorf(codes.NotFound, "no product with ID %s", req.Id)
	}
	return found, nil
}

func (p *productCatalog) SearchProducts(ctx context.Context, req *pb.SearchProductsRequest) (*pb.SearchProductsResponse, error) {
	time.Sleep(extraLatency)
	
	searchProductSpan := trace.SpanFromContext(ctx)
	searchProductSpan.SetAttributes(commonLabels...)

    traceId := searchProductSpan.SpanContext().TraceID()
	spanId := searchProductSpan.SpanContext().SpanID()

	log.Infof("TraceID: %v SpanID: %v Search product with name and description %v", traceId, spanId, strings.ToLower(req.Query))

    // Intepret query as a substring match in name or description.
	var ps []*pb.Product
	for _, p := range parseCatalog(ctx) {
		if strings.Contains(strings.ToLower(p.Name), strings.ToLower(req.Query)) ||
			strings.Contains(strings.ToLower(p.Description), strings.ToLower(req.Query)) {
			ps = append(ps, p)
			log.Infof("TraceID: %v SpanID: %v Find product %v", traceId, spanId, p)
		}
	}

	if len(ps) == 0 {
		log.Warnf("TraceID: %v SpanID: %v No product with %v", traceId, spanId, strings.ToLower(req.Query))
	} 

	return &pb.SearchProductsResponse{Results: ps}, nil
}
