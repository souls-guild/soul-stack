package grpc

import (
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"

	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
)

const (
	// moduleFetchChunkSize — размер одного PluginChunk. 256 KiB: заведомо меньше
	// send/recv-лимитов стрима, ~100 сообщений на типичный 25MB-бинарь.
	moduleFetchChunkSize = 256 * 1024

	// defaultModuleFetchPerSID — дефолтный лимит параллельных FetchModule на SID.
	defaultModuleFetchPerSID = 2
)

// sidInflight — inflight-счётчик per-SID. Ключ удаляется при нуле, карта не
// растёт с флотом.
type sidInflight struct {
	mu sync.Mutex
	n  map[string]int
}

func (l *sidInflight) tryAcquire(sid string, limit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n == nil {
		l.n = make(map[string]int)
	}
	if l.n[sid] >= limit {
		return false
	}
	l.n[sid]++
	return true
}

func (l *sidInflight) release(sid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[sid] <= 1 {
		delete(l.n, sid)
		return
	}
	l.n[sid]--
}

// FetchModule — server-streaming раздача байтов SoulModule-плагина по
// content-addressed sha256 (эпик core.module.installed, S2). Отдаются ТОЛЬКО
// sigil-allowed байты (fail-closed, [sigil.Service.LookupModuleBinary]);
// авторизация клиента — mTLS peer cert через [streamSeedAuthInterceptor],
// как у EventStream. Ошибки клиенту — без filesystem-путей.
func (h *eventStreamHandler) FetchModule(req *keeperv1.PluginFetchRequest, stream grpclib.ServerStreamingServer[keeperv1.PluginChunk]) error {
	ctx := stream.Context()
	sid, ok := authenticatedSIDFrom(ctx)
	if !ok {
		h.logger.Error("FetchModule invoked without authenticated SID — interceptor misconfigured")
		return status.Error(codes.Internal, "authentication context missing")
	}
	if h.deps.ModuleBinaries == nil {
		return status.Error(codes.Unavailable, "module fetch is not enabled")
	}

	sha := strings.ToLower(req.GetBinarySha256())
	if !isSHA256Hex(sha) {
		return status.Error(codes.InvalidArgument, "binary_sha256 must be 64 hex chars")
	}

	limit := h.deps.ModuleFetchPerSID
	if limit <= 0 {
		limit = defaultModuleFetchPerSID
	}
	if !h.fetchInflight.tryAcquire(sid, limit) {
		h.logger.Warn("fetchmodule: per-SID limit exceeded",
			slog.String("sid", sid), slog.Int("limit", limit))
		return status.Error(codes.ResourceExhausted, "too many concurrent module fetches")
	}
	defer h.fetchInflight.release(sid)

	path, err := h.deps.ModuleBinaries.LookupModuleBinary(ctx, sha)
	if err != nil {
		if errors.Is(err, sigil.ErrModuleNotAllowed) {
			h.logger.Warn("fetchmodule: module is not allowed",
				slog.String("sid", sid),
				slog.String("namespace", req.GetNamespace()),
				slog.String("name", req.GetName()),
				slog.String("binary_sha256", sha))
			return status.Error(codes.NotFound, "module is not allowed")
		}
		h.logger.Error("fetchmodule: module lookup failed",
			slog.String("sid", sid), slog.Any("error", err))
		return status.Error(codes.Unavailable, "module lookup failed")
	}

	f, err := os.Open(path)
	if err != nil {
		// Слот переехал между lookup-ом и open-ом — allowed-байтов больше нет,
		// та же категория, что не-allowed sha (fail-closed).
		h.logger.Warn("fetchmodule: allowed binary is not readable",
			slog.String("sid", sid),
			slog.String("binary_sha256", sha),
			slog.Any("error", err))
		return status.Error(codes.NotFound, "module is not allowed")
	}
	defer f.Close()

	maxBytes := h.deps.ModuleFetchMaxBytes
	if maxBytes <= 0 {
		maxBytes = int64(config.DefaultPluginMaxArtifactSizeMB) << 20
	}
	st, err := f.Stat()
	if err != nil {
		h.logger.Error("fetchmodule: stat failed",
			slog.String("sid", sid), slog.Any("error", err))
		return status.Error(codes.Internal, "module read failed")
	}
	if st.Size() > maxBytes {
		h.logger.Error("fetchmodule: binary exceeds max_artifact_size",
			slog.String("sid", sid),
			slog.String("binary_sha256", sha),
			slog.Int64("size_bytes", st.Size()),
			slog.Int64("max_bytes", maxBytes))
		return status.Error(codes.FailedPrecondition, "module binary exceeds size limit")
	}

	// stream.Send маршалит синхронно — буфер безопасно переиспользовать.
	buf := make([]byte, moduleFetchChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			if serr := stream.Send(&keeperv1.PluginChunk{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			h.logger.Error("fetchmodule: read failed",
				slog.String("sid", sid), slog.Any("error", rerr))
			return status.Error(codes.Internal, "module read failed")
		}
	}

	h.logger.Info("fetchmodule: module streamed",
		slog.String("sid", sid),
		slog.String("namespace", req.GetNamespace()),
		slog.String("name", req.GetName()),
		slog.String("binary_sha256", sha),
		slog.Int64("size_bytes", st.Size()))
	return nil
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
