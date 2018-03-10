/*
 *
 * Copyright 2014 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package grpc

import (
	"errors"
	"io"
	"sync"
	"time"

	"golang.org/x/net/context"
	"golang.org/x/net/trace"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/transport"
)

// StreamHandler defines the handler called by gRPC server to complete the
// execution of a streaming RPC.
type StreamHandler func(srv interface{}, stream ServerStream) error

// StreamDesc represents a streaming RPC service's method specification.
type StreamDesc struct {
	StreamName string
	Handler    StreamHandler

	// At least one of these is true.
	ServerStreams bool
	ClientStreams bool
}

// Stream defines the common interface a client or server stream has to satisfy.
//
// All errors returned from Stream are compatible with the status package.
type Stream interface {
	// Context returns the context for this stream.
	Context() context.Context
	// SendMsg blocks until it sends m, the stream is done or the stream
	// breaks.
	// On error, it aborts the stream and returns an RPC status on client
	// side. On server side, it simply returns the error to the caller.
	// SendMsg is called by generated code. Also Users can call SendMsg
	// directly when it is really needed in their use cases.
	// It's safe to have a goroutine calling SendMsg and another goroutine calling
	// recvMsg on the same stream at the same time.
	// But it is not safe to call SendMsg on the same stream in different goroutines.
	SendMsg(m interface{}) error
	// RecvMsg blocks until it receives a message or the stream is
	// done. On client side, it returns io.EOF when the stream is done. On
	// any other error, it aborts the stream and returns an RPC status. On
	// server side, it simply returns the error to the caller.
	// It's safe to have a goroutine calling SendMsg and another goroutine calling
	// recvMsg on the same stream at the same time.
	// But it is not safe to call RecvMsg on the same stream in different goroutines.
	RecvMsg(m interface{}) error
}

// ClientStream defines the interface a client stream has to satisfy.
type ClientStream interface {
	// Header returns the header metadata received from the server if there
	// is any. It blocks if the metadata is not ready to read.
	Header() (metadata.MD, error)
	// Trailer returns the trailer metadata from the server, if there is any.
	// It must only be called after stream.CloseAndRecv has returned, or
	// stream.Recv has returned a non-nil error (including io.EOF).
	Trailer() metadata.MD
	// CloseSend closes the send direction of the stream. It closes the stream
	// when non-nil error is met.
	CloseSend() error
	// Stream.SendMsg() may return a non-nil error when something wrong happens sending
	// the request. The returned error indicates the status of this sending, not the final
	// status of the RPC.
	//
	// Always call Stream.RecvMsg() to drain the stream and get the final
	// status, otherwise there could be leaked resources.
	Stream
}

// NewStream creates a new Stream for the client side. This is typically
// called by generated code.
func (cc *ClientConn) NewStream(ctx context.Context, desc *StreamDesc, method string, opts ...CallOption) (ClientStream, error) {
	// allow interceptor to see all applicable call options, which means those
	// configured as defaults from dial option as well as per-call options
	opts = append(cc.dopts.callOptions, opts...)

	if cc.dopts.streamInt != nil {
		return cc.dopts.streamInt(ctx, desc, cc, method, newClientStream, opts...)
	}
	return newClientStream(ctx, desc, cc, method, opts...)
}

// NewClientStream creates a new Stream for the client side. This is typically
// called by generated code.
//
// DEPRECATED: Use ClientConn.NewStream instead.
func NewClientStream(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, opts ...CallOption) (ClientStream, error) {
	return cc.NewStream(ctx, desc, method, opts...)
}

func newClientStream(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, opts ...CallOption) (_ ClientStream, err error) {
	c := defaultCallInfo()
	mc := cc.GetMethodConfig(method)
	if mc.WaitForReady != nil {
		c.failFast = !*mc.WaitForReady
	}

	// Possible context leak:
	// The cancel function for the child context we create will only be called
	// when RecvMsg returns a non-nil error, if the ClientConn is closed, or if
	// an error is generated by SendMsg.
	// https://github.com/grpc/grpc-go/issues/1818.
	var cancel context.CancelFunc
	if mc.Timeout != nil && *mc.Timeout >= 0 {
		ctx, cancel = context.WithTimeout(ctx, *mc.Timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	for _, o := range opts {
		if err := o.before(c); err != nil {
			return nil, toRPCErr(err)
		}
	}
	c.maxSendMessageSize = getMaxSize(mc.MaxReqSize, c.maxSendMessageSize, defaultClientMaxSendMessageSize)
	c.maxReceiveMessageSize = getMaxSize(mc.MaxRespSize, c.maxReceiveMessageSize, defaultClientMaxReceiveMessageSize)
	if err := setCallInfoCodec(c); err != nil {
		return nil, err
	}

	callHdr := &transport.CallHdr{
		Host:   cc.authority,
		Method: method,
		// If it's not client streaming, we should already have the request to be sent,
		// so we don't flush the header.
		// If it's client streaming, the user may never send a request or send it any
		// time soon, so we ask the transport to flush the header.
		Flush:          desc.ClientStreams,
		ContentSubtype: c.contentSubtype,
	}

	// Set our outgoing compression according to the UseCompressor CallOption, if
	// set.  In that case, also find the compressor from the encoding package.
	// Otherwise, use the compressor configured by the WithCompressor DialOption,
	// if set.
	var cp Compressor
	var comp encoding.Compressor
	if ct := c.compressorType; ct != "" {
		callHdr.SendCompress = ct
		if ct != encoding.Identity {
			comp = encoding.GetCompressor(ct)
			if comp == nil {
				return nil, status.Errorf(codes.Internal, "grpc: Compressor is not installed for requested grpc-encoding %q", ct)
			}
		}
	} else if cc.dopts.cp != nil {
		callHdr.SendCompress = cc.dopts.cp.Type()
		cp = cc.dopts.cp
	}
	if c.creds != nil {
		callHdr.Creds = c.creds
	}
	var trInfo traceInfo
	if EnableTracing {
		trInfo.tr = trace.New("grpc.Sent."+methodFamily(method), method)
		trInfo.firstLine.client = true
		if deadline, ok := ctx.Deadline(); ok {
			trInfo.firstLine.deadline = deadline.Sub(time.Now())
		}
		trInfo.tr.LazyLog(&trInfo.firstLine, false)
		ctx = trace.NewContext(ctx, trInfo.tr)
		defer func() {
			if err != nil {
				// Need to call tr.finish() if error is returned.
				// Because tr will not be returned to caller.
				trInfo.tr.LazyPrintf("RPC: [%v]", err)
				trInfo.tr.SetError()
				trInfo.tr.Finish()
			}
		}()
	}
	ctx = newContextWithRPCInfo(ctx, c.failFast)
	sh := cc.dopts.copts.StatsHandler
	if sh != nil {
		ctx = sh.TagRPC(ctx, &stats.RPCTagInfo{FullMethodName: method, FailFast: c.failFast})
		begin := &stats.Begin{
			Client:    true,
			BeginTime: time.Now(),
			FailFast:  c.failFast,
		}
		sh.HandleRPC(ctx, begin)
		defer func() {
			if err != nil {
				// Only handle end stats if err != nil.
				end := &stats.End{
					Client: true,
					Error:  err,
				}
				sh.HandleRPC(ctx, end)
			}
		}()
	}

	var (
		t    transport.ClientTransport
		s    *transport.Stream
		done func(balancer.DoneInfo)
	)
	for {
		// Check to make sure the context has expired.  This will prevent us from
		// looping forever if an error occurs for wait-for-ready RPCs where no data
		// is sent on the wire.
		select {
		case <-ctx.Done():
			return nil, toRPCErr(ctx.Err())
		default:
		}

		t, done, err = cc.getTransport(ctx, c.failFast)
		if err != nil {
			return nil, err
		}

		s, err = t.NewStream(ctx, callHdr)
		if err != nil {
			if done != nil {
				done(balancer.DoneInfo{Err: err})
				done = nil
			}
			// In the event of any error from NewStream, we never attempted to write
			// anything to the wire, so we can retry indefinitely for non-fail-fast
			// RPCs.
			if !c.failFast {
				continue
			}
			return nil, toRPCErr(err)
		}
		break
	}

	c.stream = s
	cs := &clientStream{
		opts:   opts,
		c:      c,
		desc:   desc,
		codec:  c.codec,
		cp:     cp,
		dc:     cc.dopts.dc,
		comp:   comp,
		cancel: cancel,

		done: done,
		t:    t,
		s:    s,
		p:    &parser{r: s},

		tracing: EnableTracing,
		trInfo:  trInfo,

		statsCtx:     ctx,
		statsHandler: cc.dopts.copts.StatsHandler,
	}
	if desc != unaryStreamDesc {
		// Listen on cc and stream contexts to cleanup when the user closes the
		// ClientConn or cancels the stream context.  In all other cases, an error
		// should already be injected into the recv buffer by the transport, which
		// the client will eventually receive, and then we will cancel the stream's
		// context in clientStream.finish.
		go func() {
			select {
			case <-cc.ctx.Done():
				cs.finish(ErrClientConnClosing)
			case <-ctx.Done():
				cs.finish(toRPCErr(s.Context().Err()))
			}
		}()
	}
	return cs, nil
}

// clientStream implements a client side Stream.
type clientStream struct {
	opts []CallOption
	c    *callInfo
	t    transport.ClientTransport
	s    *transport.Stream
	p    *parser
	desc *StreamDesc

	codec     baseCodec
	cp        Compressor
	dc        Decompressor
	comp      encoding.Compressor
	decomp    encoding.Compressor
	decompSet bool

	// cancel is only called when RecvMsg() returns non-nil error, which means
	// the stream finishes with error or with io.EOF.
	cancel context.CancelFunc

	tracing bool // set to EnableTracing when the clientStream is created.

	mu       sync.Mutex
	done     func(balancer.DoneInfo)
	sentLast bool // sent an end stream
	finished bool
	// trInfo.tr is set when the clientStream is created (if EnableTracing is true),
	// and is set to nil when the clientStream's finish method is called.
	trInfo traceInfo

	// statsCtx keeps the user context for stats handling.
	// All stats collection should use the statsCtx (instead of the stream context)
	// so that all the generated stats for a particular RPC can be associated in the processing phase.
	statsCtx     context.Context
	statsHandler stats.Handler
}

func (cs *clientStream) Context() context.Context {
	return cs.s.Context()
}

func (cs *clientStream) Header() (metadata.MD, error) {
	m, err := cs.s.Header()
	if err != nil {
		err = toRPCErr(err)
		cs.finish(err)
	}
	return m, err
}

func (cs *clientStream) Trailer() metadata.MD {
	return cs.s.Trailer()
}

func (cs *clientStream) SendMsg(m interface{}) (err error) {
	// TODO: Check cs.sentLast and error if we already ended the stream.
	if cs.tracing {
		cs.mu.Lock()
		if cs.trInfo.tr != nil {
			cs.trInfo.tr.LazyLog(&payload{sent: true, msg: m}, true)
		}
		cs.mu.Unlock()
	}
	// TODO Investigate how to signal the stats handling party.
	// generate error stats if err != nil && err != io.EOF?
	defer func() {
		// For non-client-streaming RPCs, we return nil instead of EOF on success
		// because the generated code requires it.  finish is not called; RecvMsg()
		// will call it with the stream's status independently.
		if err == io.EOF && !cs.desc.ClientStreams {
			err = nil
		}
		if err != nil && err != io.EOF {
			// Call finish for errors generated by this SendMsg call.  (Transport
			// errors are converted to an io.EOF error below; the real error will be
			// returned from RecvMsg eventually in that case.)
			cs.finish(err)
		}
	}()
	var outPayload *stats.OutPayload
	if cs.statsHandler != nil {
		outPayload = &stats.OutPayload{
			Client: true,
		}
	}
	hdr, data, err := encode(cs.codec, m, cs.cp, outPayload, cs.comp)
	if err != nil {
		return err
	}
	if len(data) > *cs.c.maxSendMessageSize {
		return status.Errorf(codes.ResourceExhausted, "trying to send message larger than max (%d vs. %d)", len(data), *cs.c.maxSendMessageSize)
	}
	if !cs.desc.ClientStreams {
		cs.sentLast = true
	}
	err = cs.t.Write(cs.s, hdr, data, &transport.Options{Last: !cs.desc.ClientStreams})
	if err == nil {
		if outPayload != nil {
			outPayload.SentTime = time.Now()
			cs.statsHandler.HandleRPC(cs.statsCtx, outPayload)
		}
		return nil
	}
	return io.EOF
}

func (cs *clientStream) RecvMsg(m interface{}) (err error) {
	defer func() {
		if err != nil || !cs.desc.ServerStreams {
			// err != nil or non-server-streaming indicates end of stream.
			cs.finish(err)
		}
	}()
	var inPayload *stats.InPayload
	if cs.statsHandler != nil {
		inPayload = &stats.InPayload{
			Client: true,
		}
	}
	if !cs.decompSet {
		// Block until we receive headers containing received message encoding.
		if ct := cs.s.RecvCompress(); ct != "" && ct != encoding.Identity {
			if cs.dc == nil || cs.dc.Type() != ct {
				// No configured decompressor, or it does not match the incoming
				// message encoding; attempt to find a registered compressor that does.
				cs.dc = nil
				cs.decomp = encoding.GetCompressor(ct)
			}
		} else {
			// No compression is used; disable our decompressor.
			cs.dc = nil
		}
		// Only initialize this state once per stream.
		cs.decompSet = true
	}
	err = recv(cs.p, cs.codec, cs.s, cs.dc, m, *cs.c.maxReceiveMessageSize, inPayload, cs.decomp)
	if err != nil {
		if err == io.EOF {
			if statusErr := cs.s.Status().Err(); statusErr != nil {
				return statusErr
			}
			return io.EOF // indicates successful end of stream.
		}
		return toRPCErr(err)
	}
	if cs.tracing {
		cs.mu.Lock()
		if cs.trInfo.tr != nil {
			cs.trInfo.tr.LazyLog(&payload{sent: false, msg: m}, true)
		}
		cs.mu.Unlock()
	}
	if inPayload != nil {
		cs.statsHandler.HandleRPC(cs.statsCtx, inPayload)
	}
	if cs.desc.ServerStreams {
		// Subsequent messages should be received by subsequent RecvMsg calls.
		return nil
	}

	// Special handling for non-server-stream rpcs.
	// This recv expects EOF or errors, so we don't collect inPayload.
	err = recv(cs.p, cs.codec, cs.s, cs.dc, m, *cs.c.maxReceiveMessageSize, nil, cs.decomp)
	if err == nil {
		return toRPCErr(errors.New("grpc: client streaming protocol violation: get <nil>, want <EOF>"))
	}
	if err == io.EOF {
		return cs.s.Status().Err() // non-server streaming Recv returns nil on success
	}
	return toRPCErr(err)
}

func (cs *clientStream) CloseSend() error {
	if cs.sentLast {
		return nil
	}
	cs.sentLast = true
	cs.t.Write(cs.s, nil, nil, &transport.Options{Last: true})
	// We ignore errors from Write and always return nil here.  Any error it
	// would return would also be returned by a subsequent RecvMsg call, and the
	// user is supposed to always finish the stream by calling RecvMsg until it
	// returns err != nil.
	return nil
}

func (cs *clientStream) finish(err error) {
	if err == io.EOF {
		// Ending a stream with EOF indicates a success.
		err = nil
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.finished {
		return
	}
	cs.finished = true
	cs.t.CloseStream(cs.s, err)
	for _, o := range cs.opts {
		o.after(cs.c)
	}
	if cs.done != nil {
		cs.done(balancer.DoneInfo{
			Err:           err,
			BytesSent:     true,
			BytesReceived: cs.s.BytesReceived(),
		})
		cs.done = nil
	}
	if cs.statsHandler != nil {
		end := &stats.End{
			Client:  true,
			EndTime: time.Now(),
			Error:   err,
		}
		cs.statsHandler.HandleRPC(cs.statsCtx, end)
	}
	cs.cancel()
	if !cs.tracing {
		return
	}
	if cs.trInfo.tr != nil {
		if err == nil {
			cs.trInfo.tr.LazyPrintf("RPC: [OK]")
		} else {
			cs.trInfo.tr.LazyPrintf("RPC: [%v]", err)
			cs.trInfo.tr.SetError()
		}
		cs.trInfo.tr.Finish()
		cs.trInfo.tr = nil
	}
}

// ServerStream defines the interface a server stream has to satisfy.
type ServerStream interface {
	// SetHeader sets the header metadata. It may be called multiple times.
	// When call multiple times, all the provided metadata will be merged.
	// All the metadata will be sent out when one of the following happens:
	//  - ServerStream.SendHeader() is called;
	//  - The first response is sent out;
	//  - An RPC status is sent out (error or success).
	SetHeader(metadata.MD) error
	// SendHeader sends the header metadata.
	// The provided md and headers set by SetHeader() will be sent.
	// It fails if called multiple times.
	SendHeader(metadata.MD) error
	// SetTrailer sets the trailer metadata which will be sent with the RPC status.
	// When called more than once, all the provided metadata will be merged.
	SetTrailer(metadata.MD)
	Stream
}

// serverStream implements a server side Stream.
type serverStream struct {
	t     transport.ServerTransport
	s     *transport.Stream
	p     *parser
	codec baseCodec

	cp     Compressor
	dc     Decompressor
	comp   encoding.Compressor
	decomp encoding.Compressor

	maxReceiveMessageSize int
	maxSendMessageSize    int
	trInfo                *traceInfo

	statsHandler stats.Handler

	mu sync.Mutex // protects trInfo.tr after the service handler runs.
}

func (ss *serverStream) Context() context.Context {
	return ss.s.Context()
}

func (ss *serverStream) SetHeader(md metadata.MD) error {
	if md.Len() == 0 {
		return nil
	}
	return ss.s.SetHeader(md)
}

func (ss *serverStream) SendHeader(md metadata.MD) error {
	return ss.t.WriteHeader(ss.s, md)
}

func (ss *serverStream) SetTrailer(md metadata.MD) {
	if md.Len() == 0 {
		return
	}
	ss.s.SetTrailer(md)
	return
}

func (ss *serverStream) SendMsg(m interface{}) (err error) {
	defer func() {
		if ss.trInfo != nil {
			ss.mu.Lock()
			if ss.trInfo.tr != nil {
				if err == nil {
					ss.trInfo.tr.LazyLog(&payload{sent: true, msg: m}, true)
				} else {
					ss.trInfo.tr.LazyLog(&fmtStringer{"%v", []interface{}{err}}, true)
					ss.trInfo.tr.SetError()
				}
			}
			ss.mu.Unlock()
		}
		if err != nil && err != io.EOF {
			st, _ := status.FromError(toRPCErr(err))
			ss.t.WriteStatus(ss.s, st)
		}
	}()
	var outPayload *stats.OutPayload
	if ss.statsHandler != nil {
		outPayload = &stats.OutPayload{}
	}
	hdr, data, err := encode(ss.codec, m, ss.cp, outPayload, ss.comp)
	if err != nil {
		return err
	}
	if len(data) > ss.maxSendMessageSize {
		return status.Errorf(codes.ResourceExhausted, "trying to send message larger than max (%d vs. %d)", len(data), ss.maxSendMessageSize)
	}
	if err := ss.t.Write(ss.s, hdr, data, &transport.Options{Last: false}); err != nil {
		return toRPCErr(err)
	}
	if outPayload != nil {
		outPayload.SentTime = time.Now()
		ss.statsHandler.HandleRPC(ss.s.Context(), outPayload)
	}
	return nil
}

func (ss *serverStream) RecvMsg(m interface{}) (err error) {
	defer func() {
		if ss.trInfo != nil {
			ss.mu.Lock()
			if ss.trInfo.tr != nil {
				if err == nil {
					ss.trInfo.tr.LazyLog(&payload{sent: false, msg: m}, true)
				} else if err != io.EOF {
					ss.trInfo.tr.LazyLog(&fmtStringer{"%v", []interface{}{err}}, true)
					ss.trInfo.tr.SetError()
				}
			}
			ss.mu.Unlock()
		}
		if err != nil && err != io.EOF {
			st, _ := status.FromError(toRPCErr(err))
			ss.t.WriteStatus(ss.s, st)
		}
	}()
	var inPayload *stats.InPayload
	if ss.statsHandler != nil {
		inPayload = &stats.InPayload{}
	}
	if err := recv(ss.p, ss.codec, ss.s, ss.dc, m, ss.maxReceiveMessageSize, inPayload, ss.decomp); err != nil {
		if err == io.EOF {
			return err
		}
		if err == io.ErrUnexpectedEOF {
			err = status.Errorf(codes.Internal, io.ErrUnexpectedEOF.Error())
		}
		return toRPCErr(err)
	}
	if inPayload != nil {
		ss.statsHandler.HandleRPC(ss.s.Context(), inPayload)
	}
	return nil
}

// MethodFromServerStream returns the method string for the input stream.
// The returned string is in the format of "/service/method".
func MethodFromServerStream(stream ServerStream) (string, bool) {
	s, ok := transport.StreamFromContext(stream.Context())
	if !ok {
		return "", ok
	}
	return s.Method(), ok
}
