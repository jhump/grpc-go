package grpc

import "context"

// MethodInfo contains the information of an RPC including its method name and type.
type MethodInfo struct {
	// Name is the method name only, without the service name or package name.
	Name string
	// IsClientStream indicates whether the RPC is a client streaming RPC.
	IsClientStream bool
	// IsServerStream indicates whether the RPC is a server streaming RPC.
	IsServerStream bool
}

// ServiceInfo contains unary RPC method info, streaming RPC method info and metadata for a service.
type ServiceInfo struct {
	Methods []MethodInfo
	// Metadata is the metadata specified in ServiceDesc when registering service.
	Metadata interface{}
}

// ServiceInfoForDesc returns a ServiceInfo struct with information about the
// service described by the given svc.
//
// This is an EXPERIMENTAL API.
func ServiceInfoForDesc(svc *ServiceDesc) ServiceInfo {
	methods := make([]MethodInfo, len(svc.Methods) + len(svc.Streams))
	i := 0
	for _, md := range svc.Methods {
		methods[i] = MethodInfo{Name: md.MethodName}
		i++
	}
	for _, sd := range svc.Streams {
		methods[i] = MethodInfo{
			Name:           sd.StreamName,
			IsClientStream: sd.ClientStreams,
			IsServerStream: sd.ServerStreams,
		}
		i++
	}
	return ServiceInfo{
		Methods:  methods,
		Metadata: svc.Metadata,
	}
}

type svcInfoKey struct{}

// ServiceInfoFromContext queries for information about the service that
// contains the currently method currenty being invoked. This may be called from
// interceptors or server handlers to get metadata about the current service.
// This can even be used from client interceptors, but may return nil if the
// generated client stub for the service was generated with an older version of
// protoc-gen-go.
//
// This is an EXPERIMENTAL API.
func ServiceInfoFromContext(ctx context.Context) *ServiceInfo {
	si, _ := ctx.Value(svcInfoKey{}).(*ServiceInfo)
	return si
}

// NewContextWithServiceInfo stores the given service description into a new
// context, for subsequent use with ServiceInfoFromContext.
//
// This is an EXPERIMENTAL API.
func NewContextWithServiceInfo(ctx context.Context, si *ServiceInfo) context.Context {
	return context.WithValue(ctx, svcInfoKey{}, si)
}
