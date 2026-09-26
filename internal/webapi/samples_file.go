package webapi

import (
	"context"
	"os"
	"time"

	"github.com/wlame/rx-go/internal/config"
	"github.com/wlame/rx-go/internal/index"
	"github.com/wlame/rx-go/internal/samples"
	"github.com/wlame/rx-go/pkg/rxtypes"
)

// fileSamples is one file's samples answer over HTTP: the answer, or
// the task a request that prefers respond-async is told to follow
// instead (202), and the background build an answer from the head of
// the file started or joined (index_build).
type fileSamples struct {
	resp       *rxtypes.SamplesResponse
	pending    *rxtypes.TaskResponse
	indexBuild *rxtypes.SamplesIndexBuild
}

// readFileSamples answers req, a samples request on one pinned file
// whose kind req carries, the way GET /v1/samples answers it; stat is a
// current stat of the file, and deadline gives the wait's timer (a nil
// channel waits as long as the build).
//
// A file that wants a line index and has none (samples.ShouldBuildIndex)
// is answered from its head when the head holds the answer, and the
// build is started or joined in the background; otherwise the request
// waits for the build until the deadline (pending is then set, and the
// caller answers 202). Any other request reads the file, with its index
// when one is current. RX_NO_INDEX (noIndex) builds and waits for
// nothing.
//
// The errors are samples.Resolve's, and the wait's when the request's
// context ends first.
func (s *Server) readFileSamples(ctx context.Context, req samples.Request, stat os.FileInfo, noIndex bool,
	deadline func() (<-chan time.Time, func())) (fileSamples, error) {
	var (
		out      fileSamples
		answered bool
		reach    *samples.HeadReach
		err      error
	)
	wantsBuild := !noIndex && samples.ShouldBuildIndex(req.Path, *req.Kind, stat.Size())
	if wantsBuild {
		// The running build keeps how far the head reaches once a lookup
		// has run past it, so a lookup the head is known not to hold
		// reads nothing before it waits.
		known := s.samplesIndex.reachOf(req.Path, index.IdentityFromInfo(req.Path, stat))
		out.resp, answered, reach, err = samples.ResolveFromHeadWithReach(ctx, req, config.SamplesHeadBytes(), known)
		if answered && err == nil {
			out.indexBuild = s.samplesIndex.start(req.Path, stat)
		}
	}
	if wantsBuild && !answered {
		timer, stop := deadline()
		pending, waitErr := s.samplesIndex.await(ctx, req.Path, stat, reach, timer)
		stop()
		if waitErr != nil {
			return out, waitErr
		}
		if pending != nil {
			out.pending = pending
			return out, nil
		}
	}
	if !answered {
		// ctx ends when the client disconnects, which stops the read.
		out.resp, err = samples.Resolve(ctx, req)
	}
	return out, err
}
