package crproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/distribution/distribution/v3/registry/api/errcode"
)

func blobCachePath(blob string) string {
	blob = strings.TrimPrefix(blob, "sha256:")
	return path.Join("/docker/registry/v2/blobs/sha256", blob[:2], blob, "data")
}

func (c *CRProxy) cacheBlobResponse(rw http.ResponseWriter, r *http.Request, info *PathInfo) {
	ctx := r.Context()

	blobPath := blobCachePath(info.Blobs)

	closeValue, loaded := c.mutCache.LoadOrStore(blobPath, make(chan struct{}))
	closeCh := closeValue.(chan struct{})
	for loaded {
		select {
		case <-ctx.Done():
			err := ctx.Err().Error()
			if c.logger != nil {
				c.logger.Println(err)
			}
			http.Error(rw, err, http.StatusInternalServerError)
			return
		case <-closeCh:
		}
		closeValue, loaded = c.mutCache.LoadOrStore(blobPath, make(chan struct{}))
		closeCh = closeValue.(chan struct{})
	}

	doneCache := func() {
		c.mutCache.Delete(blobPath)
		close(closeCh)
	}

	stat, err := c.storageDriver.Stat(ctx, blobPath)
	if err == nil {
		doneCache()

		size := stat.Size()
		if r.Method == http.MethodHead {
			rw.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			rw.Header().Set("Content-Type", "application/octet-stream")
			return
		}

		if !c.isPrivileged(r.RemoteAddr) {
			c.accumulativeLimit(r, info, size)
			if !c.waitForLimit(r, info, size) {
				c.errorResponse(rw, r, nil)
				return
			}
		}

		err = c.redirect(rw, r, blobPath)
		if err == nil {
			return
		}
		c.errorResponse(rw, r, ctx.Err())
		return
	}
	if c.logger != nil {
		c.logger.Println("Cache miss", blobPath)
	}

	type repo struct {
		err  error
		size int64
	}
	signalCh := make(chan repo, 1)

	go func() {
		defer doneCache()
		size, err := c.cacheBlobContent(context.Background(), r, blobPath, info)
		signalCh <- repo{
			err:  err,
			size: size,
		}
	}()

	select {
	case <-ctx.Done():
		c.errorResponse(rw, r, ctx.Err())
		return
	case signal := <-signalCh:
		if signal.err != nil {
			c.errorResponse(rw, r, signal.err)
			return
		}
		if r.Method == http.MethodHead {
			rw.Header().Set("Content-Length", strconv.FormatInt(signal.size, 10))
			rw.Header().Set("Content-Type", "application/octet-stream")
			return
		}

		if !c.isPrivileged(r.RemoteAddr) {
			c.accumulativeLimit(r, info, signal.size)
			if !c.waitForLimit(r, info, signal.size) {
				c.errorResponse(rw, r, nil)
				return
			}
		}

		err = c.redirect(rw, r, blobPath)
		if err != nil {
			if c.logger != nil {
				c.logger.Println("failed to redirect", blobPath, err)
			}
		}
		return
	}
}

func (c *CRProxy) cacheBlobContent(ctx context.Context, r *http.Request, blobPath string, info *PathInfo) (int64, error) {
	cli := c.getClientset(info.Host, info.Image)
	resp, err := c.doWithAuth(cli, r.WithContext(ctx), info.Host)
	if err != nil {
		return 0, err
	}
	defer func() {
		resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return 0, errcode.ErrorCodeDenied
		}
		return 0, errcode.ErrorCodeUnknown.WithMessage(fmt.Sprintf("Source response code %s", resp.StatusCode))
	}

	buf := c.bytesPool.Get().([]byte)
	defer c.bytesPool.Put(buf)

	fw, err := c.storageDriver.Writer(ctx, blobPath, false)
	if err != nil {
		return 0, err
	}

	h := sha256.New()
	n, err := io.CopyBuffer(fw, io.TeeReader(resp.Body, h), buf)
	if err != nil {
		fw.Cancel()
		return 0, err
	}

	if n != resp.ContentLength {
		fw.Cancel()
		return 0, fmt.Errorf("expected %d bytes, got %d", resp.ContentLength, n)
	}

	hash := hex.EncodeToString(h.Sum(nil)[:])
	if info.Blobs[7:] != hash {
		fw.Cancel()
		return 0, fmt.Errorf("expected %s hash, got %s", info.Blobs[7:], hash)
	}

	err = fw.Commit()
	if err != nil {
		return 0, err
	}
	return n, nil
}
