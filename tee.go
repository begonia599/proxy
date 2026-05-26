// tee.go: 旁路一份响应 body 给后台 goroutine 用，不影响主转发
package main

import (
	"bytes"
	"io"
	"sync"
)

// teeBody 包一层 io.ReadCloser，边读边把字节复制进内存 buf，
// Close 或 io.EOF 时把 buf 交给 onClose 回调（异步）。
// 4MB 上限防 streaming 响应把内存吃爆——usage 信息只藏在
// message_start / message_delta 两个事件里，远不到 4MB。
type teeBody struct {
	src     io.ReadCloser
	buf     *bytes.Buffer
	maxBuf  int
	onClose func([]byte)
	once    sync.Once
}

func newTeeBody(src io.ReadCloser, onClose func([]byte)) *teeBody {
	return &teeBody{
		src:     src,
		buf:     &bytes.Buffer{},
		maxBuf:  4 << 20,
		onClose: onClose,
	}
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 && t.buf.Len() < t.maxBuf {
		remain := t.maxBuf - t.buf.Len()
		w := n
		if w > remain {
			w = remain
		}
		t.buf.Write(p[:w])
	}
	if err != nil {
		t.fire()
	}
	return n, err
}

func (t *teeBody) Close() error {
	t.fire()
	return t.src.Close()
}

func (t *teeBody) fire() {
	t.once.Do(func() {
		captured := append([]byte(nil), t.buf.Bytes()...)
		go t.onClose(captured)
	})
}
