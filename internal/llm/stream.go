// 本文件定义供应商适配器返回的有序响应流接口。
package llm

import "context"

// ResponseStream 按顺序提供一次或多次助手响应的增量事件。
type ResponseStream interface {
	// Recv 返回下一个响应事件；流结束时返回 io.EOF。
	Recv(context.Context) (ResponseEvent, error)
}
