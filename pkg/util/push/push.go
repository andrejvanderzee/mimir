// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/util/push/push.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package push

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/go-kit/log/level"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/middleware"

	"github.com/grafana/mimir/pkg/mimirpb"
	"github.com/grafana/mimir/pkg/util"
	"github.com/grafana/mimir/pkg/util/globalerror"
	"github.com/grafana/mimir/pkg/util/log"
)

// Func defines the type of the push. It is similar to http.HandlerFunc.
type Func func(ctx context.Context, req *mimirpb.WriteRequest, cleanup func()) (*mimirpb.WriteResponse, error)

// ParserFunc defines the parser code.
type ParserFunc func(ctx context.Context, r *http.Request, maxSize int, buffer []byte, req *mimirpb.PreallocWriteRequest) ([]byte, error)

// Wrap a slice in a struct so we can store a pointer in sync.Pool
type bufHolder struct {
	buf []byte
}

var bufferPool = sync.Pool{
	New: func() interface{} { return &bufHolder{buf: make([]byte, 256*1024)} },
}

const SkipLabelNameValidationHeader = "X-Mimir-SkipLabelNameValidation"
const statusClientClosedRequest = 499

// Handler is a http.Handler which accepts WriteRequests.
func Handler(
	maxRecvMsgSize int,
	sourceIPs *middleware.SourceIPExtractor,
	allowSkipLabelNameValidation bool,
	push Func,
) http.Handler {
	return handler(maxRecvMsgSize, sourceIPs, allowSkipLabelNameValidation, push, func(ctx context.Context, r *http.Request, maxRecvMsgSize int, dst []byte, req *mimirpb.PreallocWriteRequest) ([]byte, error) {
		res, err := util.ParseProtoReader(ctx, r.Body, int(r.ContentLength), maxRecvMsgSize, dst, req, util.RawSnappy)
		if errors.Is(err, util.MsgSizeTooLargeErr{}) {
			err = distributorMaxWriteMessageSizeErr{actual: int(r.ContentLength), limit: maxRecvMsgSize}
		}
		return res, err
	})
}

type distributorMaxWriteMessageSizeErr struct {
	actual, limit int
}

func (e distributorMaxWriteMessageSizeErr) Error() string {
	msgSizeDesc := fmt.Sprintf(" of %d bytes", e.actual)
	if e.actual < 0 {
		msgSizeDesc = ""
	}
	return globalerror.DistributorMaxWriteMessageSize.MessageWithPerInstanceLimitConfig(fmt.Sprintf("the incoming push request has been rejected because its message size%s is larger than the allowed limit of %d bytes", msgSizeDesc, e.limit), "distributor.max-recv-msg-size")
}

// handler requires an additional parser argument.
func handler(maxRecvMsgSize int,
	sourceIPs *middleware.SourceIPExtractor,
	allowSkipLabelNameValidation bool,
	push Func,
	parser ParserFunc,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := log.WithContext(ctx, log.Logger)
		if sourceIPs != nil {
			source := sourceIPs.Get(r)
			if source != "" {
				ctx = util.AddSourceIPsToOutgoingContext(ctx, source)
				logger = log.WithSourceIPs(source, logger)
			}
		}
		bufHolder := bufferPool.Get().(*bufHolder)
		var req mimirpb.PreallocWriteRequest
		buf, err := parser(ctx, r, maxRecvMsgSize, bufHolder.buf, &req)
		if err != nil {
			level.Error(logger).Log("err", err.Error())

			// Check for httpgrpc error.
			if resp, ok := httpgrpc.HTTPResponseFromError(err); ok {
				http.Error(w, string(resp.Body), int(resp.Code))
			} else {
				http.Error(w, err.Error(), http.StatusBadRequest)
			}

			bufferPool.Put(bufHolder)
			return
		}
		// If decoding allocated a bigger buffer, put that one back in the pool.
		if len(buf) > len(bufHolder.buf) {
			bufHolder.buf = buf
		}

		cleanup := func() {
			mimirpb.ReuseSlice(req.Timeseries)
			bufferPool.Put(bufHolder)
		}

		if allowSkipLabelNameValidation {
			req.SkipLabelNameValidation = req.SkipLabelNameValidation && r.Header.Get(SkipLabelNameValidationHeader) == "true"
		} else {
			req.SkipLabelNameValidation = false
		}

		if req.Source == 0 {
			req.Source = mimirpb.API
		}

		if _, err := push(ctx, &req.WriteRequest, cleanup); err != nil {
			if errors.Is(err, context.Canceled) {
				http.Error(w, err.Error(), statusClientClosedRequest)
				level.Warn(logger).Log("msg", "push request canceled", "err", err)
				return
			}
			resp, ok := httpgrpc.HTTPResponseFromError(err)
			if !ok {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if resp.GetCode() != 202 {
				level.Error(logger).Log("msg", "push error", "err", err)
			}
			http.Error(w, string(resp.Body), int(resp.Code))
		}
	})
}
