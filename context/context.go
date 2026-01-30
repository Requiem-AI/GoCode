package context

import (
	context2 "context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog/log"
)

// Context is a small service wrapper that handles the startup/shutdown of the service
// Services are stored in the order passed to it, as slices dont preserve order maps are used.
// Provides cross-service access while still maintaining separation of concerns
type Context struct {
	startOrder map[int]string
	serviceMap map[string]Service
}

// NewCtx Create a new context containing the given services.
func NewCtx(svcs ...Service) (*Context, error) {
	ctx := Context{
		startOrder: make(map[int]string, len(svcs)),
		serviceMap: make(map[string]Service, len(svcs)),
	}

	for _, s := range svcs {
		if err := ctx.Register(s); err != nil {
			return nil, err
		}
	}

	return &ctx, nil
}

// Register a new service into the context and preseve the order passed
func (ctx *Context) Register(service Service) error {
	if _, ok := ctx.serviceMap[service.Id()]; ok {
		return fmt.Errorf("service %s already registered", service.Id())
	}

	currLen := len(ctx.serviceMap) //Starts from 0

	ctx.startOrder[currLen] = service.Id()
	ctx.serviceMap[service.Id()] = service

	return nil
}

// Service Returns the pointer to the given service.
// Note: once returned the service must be cast to the correct service
// Example: ctx.Service(DATASTORE_SERVICE).(*Datastore)
func (ctx *Context) Service(id string) Service {
	return ctx.serviceMap[id]
}

// Run Starts the context
// Each service is configured first, if any fail here the context will bail out
// Each service is started, if any fail here the context will bail out
func (ctx *Context) Run() error {
	// Create a context that is canceled on SIGINT or SIGTERM
	_, cancel := context2.WithCancel(context2.Background())
	defer cancel()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start a goroutine that will wait for a signal
	go func() {
		sig := <-sigChan
		log.Info().Str("signal", sig.String()).Msg("Received signal. Shutting down")

		for i := len(ctx.startOrder) - 1; i >= 0; i-- {
			svcId := ctx.startOrder[i]
			log.Info().Str("service", svcId).Msg("Shutting down")
			ctx.serviceMap[svcId].Shutdown()
		}
		cancel()
	}()

	for i := 0; i < len(ctx.startOrder); i++ {
		svcId := ctx.startOrder[i]

		if err := ctx.Configure(ctx.serviceMap[svcId]); err != nil {
			log.Fatal().Err(err).Str("service", svcId).Msg("Context Configure Error")
			return err
		}
	}

	for i := 0; i < len(ctx.startOrder); i++ {
		svcId := ctx.startOrder[i]

		if err := ctx.Start(ctx.serviceMap[svcId]); err != nil {
			log.Fatal().Err(err).Str("service", svcId).Msg("Context Start Error")
			return err
		}
	}

	return nil
}

// Configure the given service
func (ctx *Context) Configure(svc Service) error {
	log.Info().Str("service", svc.Id()).Msg("Context Configure")

	if err := svc.Configure(ctx); err != nil {
		return err
	}

	return nil
}

// Start the given service
func (ctx *Context) Start(svc Service) error {
	log.Info().Str("service", svc.Id()).Msg("Context Start")

	if err := svc.Start(); err != nil {
		return err
	}

	return nil
}

func (ctx *Context) Services() []string {
	var keys []string
	for k := range ctx.serviceMap {
		keys = append(keys, k)
	}

	return keys
}
