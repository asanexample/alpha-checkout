// Command server is app-alpha-checkout: a generic starter service for the platform.
//
// It is deliberately minimal — a stdlib-only HTTP server exposing the liveness/readiness endpoint the
// platform deployment manifests probe (/healthz) and a JSON root handler. There is NO cloud/AWS
// dependency: an environment's AWS access (if any) is granted out-of-band via EKS Pod Identity to the named
// ServiceAccount (see k8s/preprod/serviceaccount.yaml) and declared in the Environment claim's `aws` block.
// Add an SDK + the access only when an app actually needs it.
//
// To start a NEW app from this template: copy the repo, rename app-alpha-checkout -> app-<yourapp>, set your
// team/namespace/hostname in k8s/preprod/, and keep the thin .github/workflows callers as-is.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// newMux wires the routes — extracted so the unit test can exercise them without binding a port.
func newMux(version, namespace string) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"app":       "app-alpha-checkout",
			"version":   version,
			"namespace": namespace,
			"hostname":  r.Host,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// /checkout confirms an "order" — the downstream service alpha-shop calls to demonstrate a real
	// east-west (service-to-service) call. Registered for GET and POST (ServeMux treats them as distinct
	// patterns); GET keeps the demo a trivial curl. `host` is the pod name, so the caller can see which
	// replica answered. ADR-057 Phase 2 later requires this path to be mutually authenticated.
	host, _ := os.Hostname()
	checkout := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"order":       fmt.Sprintf("%04d", time.Now().Unix()%10000),
			"confirmedBy": "app-alpha-checkout",
			"host":        host,
		})
	}
	mux.HandleFunc("GET /checkout", checkout)
	mux.HandleFunc("POST /checkout", checkout)

	return mux
}

func main() {
	version := getenv("VERSION", "dev")
	namespace := getenv("NAMESPACE", "unknown")

	// OpenTelemetry (ADR-077 / P14): extract the W3C traceparent that alpha-shop propagates and open a
	// server span in the SAME trace, exporting to the platform OTLP collector. The endpoint comes from the
	// injected env (the OTel Operator's inject-sdk annotation) — never hardcoded. Degrades cleanly (silent
	// export failures) if OTEL_EXPORTER_OTLP_ENDPOINT is unset, e.g. local/test runs.
	if shutdownTracer, err := initTracer(context.Background()); err != nil {
		log.Printf("otel init failed; continuing without tracing: %v", err)
	} else {
		defer func() { _ = shutdownTracer(context.Background()) }()
	}

	// otelhttp.NewHandler extracts the incoming trace context and opens a server span per request, so
	// checkout's spans join shop's distributed trace (and show as a node/edge in the service graph).
	handler := otelhttp.NewHandler(newMux(version, namespace), "http.server")

	srv := &http.Server{
		Addr:         ":8080",
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Serve in the background; block on SIGTERM/SIGINT (k8s sends SIGTERM on pod termination), then drain
	// in-flight requests gracefully before exiting.
	go func() {
		log.Printf("starting app-alpha-checkout version=%s namespace=%s", version, namespace)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("shutting down (draining in-flight requests)…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("graceful shutdown failed: %v", err)
	}
	log.Println("stopped")
}

// initTracer sets up the global tracer provider + W3C propagator with an OTLP/HTTP exporter. Returns a
// shutdown func that flushes the batch processor. If OTEL_EXPORTER_OTLP_ENDPOINT is unset the exporter
// still constructs (defaults to localhost) but export failures are silent — fine for local/test runs.
func initTracer(ctx context.Context) (func(context.Context) error, error) {
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(), // OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES
		resource.WithAttributes(semconv.ServiceName(getenv("OTEL_SERVICE_NAME", "app-alpha-checkout"))),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp.Shutdown, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
