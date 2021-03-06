package manager

import (
	"time"

	gorun "github.com/micro/go-micro/v3/runtime"
	"github.com/micro/micro/v3/internal/namespace"
	"github.com/micro/micro/v3/service/logger"
	"github.com/micro/micro/v3/service/runtime"
	"github.com/micro/micro/v3/service/runtime/builder"
	"github.com/micro/micro/v3/service/runtime/manager/util"
)

// Create registers a service
func (m *manager) Create(srv *runtime.Service, opts ...runtime.CreateOption) error {
	// parse the options
	var options runtime.CreateOptions
	for _, o := range opts {
		o(&options)
	}
	if len(options.Namespace) == 0 {
		options.Namespace = namespace.DefaultNamespace
	}

	// set defaults
	if srv.Metadata == nil {
		srv.Metadata = make(map[string]string)
	}
	if len(srv.Version) == 0 {
		srv.Version = "latest"
	}

	// construct the service object
	service := &service{
		Service:   srv,
		Options:   &options,
		UpdatedAt: time.Now(),
	}

	// if there is not a builder configured, start the service and then write it to the store
	if builder.DefaultBuilder == nil {
		// the source could be a git remote or a reference to the blob store, parse it before we run
		// the service
		var err error
		srv.Source, err = m.checkoutSource(service)
		if err != nil {
			return err
		}

		// create the service in the underlying runtime
		if err := m.createServiceInRuntime(service); err != nil && err != runtime.ErrAlreadyExists {
			return err
		}

		// write the object to the store
		return m.writeService(service)
	}

	// building ths service can take some time so we'll write the service to the store and then
	// perform the build process async
	service.Status = gorun.Pending
	if err := m.writeService(service); err != nil {
		return err
	}

	go m.buildAndRun(service)
	return nil
}

// Read returns the service which matches the criteria provided
func (m *manager) Read(opts ...runtime.ReadOption) ([]*runtime.Service, error) {
	// parse the options
	var options runtime.ReadOptions
	for _, o := range opts {
		o(&options)
	}
	if len(options.Namespace) == 0 {
		options.Namespace = namespace.DefaultNamespace
	}

	// query the store
	srvs, err := m.readServices(options.Namespace, &runtime.Service{
		Name:    options.Service,
		Version: options.Version,
	})
	if err != nil {
		return nil, err
	}

	// query the runtime and group the resulting services by name:version so they can be queried
	rSrvs, err := m.Runtime.Read(opts...)
	if err != nil {
		return nil, err
	}
	rSrvMap := make(map[string]*runtime.Service, len(rSrvs))
	for _, s := range rSrvs {
		rSrvMap[s.Name+":"+s.Version] = s
	}

	// loop through the services returned from the store and append any info returned by the runtime
	// such as status and error
	result := make([]*runtime.Service, len(srvs))
	for i, s := range srvs {
		result[i] = s.Service

		// check for a status on the service, this could be building, stopping etc
		if s.Status != gorun.Unknown {
			result[i].Status = s.Status
		}
		if len(s.Error) > 0 {
			result[i].Metadata["error"] = s.Error
		}

		// set the last updated, todo: check why this is 'started' and not 'updated'. Consider adding
		// this as an attribute on runtime.Service
		if !s.UpdatedAt.IsZero() {
			result[i].Metadata["started"] = s.UpdatedAt.Format(time.RFC3339)
		}

		// the service might still be building and not have been created in the underlying runtime yet
		rs, ok := rSrvMap[s.Service.Name+":"+s.Service.Version]
		if !ok {
			continue
		}

		// assign the status and error. TODO: make the error an attribute on service
		result[i].Status = rs.Status
		if rs.Metadata != nil && len(rs.Metadata["error"]) > 0 {
			result[i].Metadata["status"] = rs.Metadata["error"]
		}
	}

	return result, nil
}

// Update the service in place
func (m *manager) Update(srv *runtime.Service, opts ...runtime.UpdateOption) error {
	// parse the options
	var options runtime.UpdateOptions
	for _, o := range opts {
		o(&options)
	}
	if len(options.Namespace) == 0 {
		options.Namespace = namespace.DefaultNamespace
	}

	// set default
	if len(srv.Version) == 0 {
		srv.Version = "latest"
	}

	// read the service from the store
	srvs, err := m.readServices(options.Namespace, &runtime.Service{
		Name:    srv.Name,
		Version: srv.Version,
	})
	if err != nil {
		return err
	}
	if len(srvs) == 0 {
		return gorun.ErrNotFound
	}

	// update the service
	service := srvs[0]
	service.Service.Source = srv.Source
	service.UpdatedAt = time.Now()

	// if there is not a builder configured, update the service and then write it to the store
	if builder.DefaultBuilder == nil {
		// the source could be a git remote or a reference to the blob store, parse it before we run
		// the service
		var err error
		service.Service.Source, err = m.checkoutSource(service)
		if err != nil {
			return err
		}

		// create the service in the underlying runtime
		if err := m.updateServiceInRuntime(service); err != nil {
			return err
		}

		// write the object to the store
		service.Status = runtime.Starting
		service.Error = ""
		return m.writeService(service)
	}

	// building ths service can take some time so we'll write the service to the store and then
	// perform the build process async
	service.Status = gorun.Pending
	if err := m.writeService(service); err != nil {
		return err
	}

	go m.buildAndUpdate(service)
	return nil
}

// Delete a service
func (m *manager) Delete(srv *runtime.Service, opts ...runtime.DeleteOption) error {
	// parse the options
	var options runtime.DeleteOptions
	for _, o := range opts {
		o(&options)
	}
	if len(options.Namespace) == 0 {
		options.Namespace = namespace.DefaultNamespace
	}

	// set defaults
	if len(srv.Version) == 0 {
		srv.Version = "latest"
	}

	// read the service from the store
	srvs, err := m.readServices(options.Namespace, &runtime.Service{
		Name:    srv.Name,
		Version: srv.Version,
	})
	if err != nil {
		return err
	}
	if len(srvs) == 0 {
		return gorun.ErrNotFound
	}

	// delete from the underlying runtime
	if err := m.Runtime.Delete(srv, opts...); err != nil && err != runtime.ErrNotFound {
		return err
	}

	// delete from the store
	if err := m.deleteService(srvs[0]); err != nil {
		return err
	}

	// delete the source and binary from the blob store async
	go m.cleanupBlobStore(srvs[0])
	return nil
}

// Starts the manager
func (m *manager) Start() error {
	if m.running {
		return nil
	}
	m.running = true

	// start the runtime we're going to manage
	if err := runtime.DefaultRuntime.Start(); err != nil {
		return err
	}

	// Watch services that were running previously. TODO: rename and run periodically
	go m.watchServices()

	return nil
}

func (m *manager) watchServices() {
	nss, err := m.listNamespaces()
	if err != nil {
		logger.Warnf("Error listing namespaces: %v", err)
		return
	}

	for _, ns := range nss {
		srvs, err := m.readServices(ns, &runtime.Service{})
		if err != nil {
			logger.Warnf("Error reading services from the %v namespace: %v", ns, err)
			return
		}

		running := map[string]*runtime.Service{}
		curr, _ := runtime.Read(runtime.ReadNamespace(ns))
		for _, v := range curr {
			running[v.Name+":"+v.Version] = v
		}

		for _, srv := range srvs {
			// already running, don't need to start again
			if _, ok := running[srv.Service.Name+":"+srv.Service.Version]; ok {
				continue
			}

			// skip services which aren't running for a reason
			if srv.Status == gorun.Error {
				continue
			}
			if srv.Status == gorun.Building {
				continue
			}
			if srv.Status == gorun.Stopped {
				continue
			}

			// create the service
			if err := m.createServiceInRuntime(srv); err != nil {
				if logger.V(logger.ErrorLevel, logger.DefaultLogger) {
					logger.Errorf("Error restarting service: %v", err)
				}
			}
		}
	}
}

// Stop the manager
func (m *manager) Stop() error {
	if !m.running {
		return nil
	}
	m.running = false

	return runtime.DefaultRuntime.Stop()
}

// String describes runtime
func (m *manager) String() string {
	return "manager"
}

type manager struct {
	// running is true after Start is called
	running bool

	gorun.Runtime
}

// New returns a manager for the runtime
func New() gorun.Runtime {
	return &manager{
		Runtime: util.NewCache(runtime.DefaultRuntime),
	}
}
