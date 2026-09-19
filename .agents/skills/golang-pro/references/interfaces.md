# Interface 设计与组合

## 小而专注的 interface

```go
// 单方法 interface（idiomatic Go）
type Reader interface {
    Read(p []byte) (n int, err error)
}

type Writer interface {
    Write(p []byte) (n int, err error)
}

type Closer interface {
    Close() error
}

// interface 组合
type ReadCloser interface {
    Reader
    Closer
}

type WriteCloser interface {
    Writer
    Closer
}

type ReadWriteCloser interface {
    Reader
    Writer
    Closer
}
```

## 收 interface，返回 struct

```go
package storage

import "io"

// Storage 是具体类型（struct）
type Storage struct {
    baseDir string
}

// NewStorage 返回具体类型
func NewStorage(baseDir string) *Storage {
    return &Storage{baseDir: baseDir}
}

// SaveFile 接收 interface 以获得灵活性
func (s *Storage) SaveFile(filename string, data io.Reader) error {
    // 实现可以处理任何 Reader
    //（文件、网络、buffer 等）
    return nil
}

// 用法上支持依赖注入
type Uploader interface {
    SaveFile(filename string, data io.Reader) error
}

type Service struct {
    uploader Uploader // 收 interface
}

// NewService 收 interface，方便测试
func NewService(uploader Uploader) *Service {
    return &Service{uploader: uploader}
}
```

## io.Reader 与 io.Writer 模式

```go
import (
    "io"
    "strings"
)

// 用 io.MultiReader 串接 reader
func combineReaders() io.Reader {
    r1 := strings.NewReader("Hello ")
    r2 := strings.NewReader("World")
    return io.MultiReader(r1, r2)
}

// 用 tee reader 复制读取流
func duplicateRead(r io.Reader, w io.Writer) io.Reader {
    return io.TeeReader(r, w) // 从 r 读的同时写入 w
}

// 限制 reader 防止读超量
func limitedRead(r io.Reader, n int64) io.Reader {
    return io.LimitReader(r, n)
}

// 自定义 Reader 实现
type UppercaseReader struct {
    src io.Reader
}

func (u *UppercaseReader) Read(p []byte) (n int, err error) {
    n, err = u.src.Read(p)
    for i := 0; i < n; i++ {
        if p[i] >= 'a' && p[i] <= 'z' {
            p[i] = p[i] - 32
        }
    }
    return n, err
}

// 自定义 Writer 实现
type CountingWriter struct {
    w     io.Writer
    count int64
}

func (cw *CountingWriter) Write(p []byte) (n int, err error) {
    n, err = cw.w.Write(p)
    cw.count += int64(n)
    return n, err
}

func (cw *CountingWriter) BytesWritten() int64 {
    return cw.count
}
```

## 用内嵌做组合

```go
import "sync"

// 内嵌以扩展行为
type SafeCounter struct {
    mu sync.Mutex
    m  map[string]int
}

func (sc *SafeCounter) Inc(key string) {
    sc.mu.Lock()
    defer sc.mu.Unlock()
    sc.m[key]++
}

// 内嵌 interface 以提供默认行为
type Logger interface {
    Log(msg string)
}

type NoOpLogger struct{}

func (NoOpLogger) Log(msg string) {}

type Service struct {
    Logger // 内嵌 interface（可提供默认实现）
}

func NewService(logger Logger) *Service {
    if logger == nil {
        logger = NoOpLogger{} // 提供默认值
    }
    return &Service{Logger: logger}
}

// 现在 Service.Log() 可用
```

## interface 实现校验

```go
import "io"

// 编译期 interface 校验
var _ io.Reader = (*MyReader)(nil)
var _ io.Writer = (*MyWriter)(nil)
var _ io.Closer = (*MyCloser)(nil)

type MyReader struct{}

func (m *MyReader) Read(p []byte) (n int, err error) {
    return 0, nil
}

type MyWriter struct{}

func (m *MyWriter) Write(p []byte) (n int, err error) {
    return len(p), nil
}

type MyCloser struct{}

func (m *MyCloser) Close() error {
    return nil
}
```

## functional options 模式

```go
package server

import "time"

type Server struct {
    host         string
    port         int
    timeout      time.Duration
    maxConns     int
    enableLogger bool
}

// Option 是配置 Server 的 functional option
type Option func(*Server)

func WithHost(host string) Option {
    return func(s *Server) {
        s.host = host
    }
}

func WithPort(port int) Option {
    return func(s *Server) {
        s.port = port
    }
}

func WithTimeout(timeout time.Duration) Option {
    return func(s *Server) {
        s.timeout = timeout
    }
}

func WithMaxConnections(max int) Option {
    return func(s *Server) {
        s.maxConns = max
    }
}

func WithLogger(enabled bool) Option {
    return func(s *Server) {
        s.enableLogger = enabled
    }
}

// NewServer 用 functional options 创建 server
func NewServer(opts ...Option) *Server {
    // 默认值
    s := &Server{
        host:     "localhost",
        port:     8080,
        timeout:  30 * time.Second,
        maxConns: 100,
    }

    // 应用选项
    for _, opt := range opts {
        opt(s)
    }

    return s
}

// 用法：
// server := NewServer(
//     WithHost("0.0.0.0"),
//     WithPort(9000),
//     WithTimeout(60 * time.Second),
//     WithLogger(true),
// )
```

## interface 隔离

```go
// 反例：臃肿 interface
type BadRepository interface {
    Create(item Item) error
    Read(id string) (Item, error)
    Update(item Item) error
    Delete(id string) error
    List() ([]Item, error)
    Search(query string) ([]Item, error)
    Count() (int, error)
}

// 正例：隔离的 interface
type Creator interface {
    Create(item Item) error
}

type Reader interface {
    Read(id string) (Item, error)
}

type Updater interface {
    Update(item Item) error
}

type Deleter interface {
    Delete(id string) error
}

type Lister interface {
    List() ([]Item, error)
}

// 只组合需要的部分
type ReadWriter interface {
    Reader
    Creator
}

type FullRepository interface {
    Creator
    Reader
    Updater
    Deleter
    Lister
}
```

## 类型断言与 type switch

```go
import "fmt"

// 安全的类型断言
func processValue(v interface{}) {
    // 双值断言（安全）
    if str, ok := v.(string); ok {
        fmt.Println("String:", str)
        return
    }

    // type switch
    switch val := v.(type) {
    case int:
        fmt.Println("Int:", val)
    case string:
        fmt.Println("String:", val)
    case bool:
        fmt.Println("Bool:", val)
    default:
        fmt.Println("Unknown type")
    }
}

// 检查可选 interface 方法
type Flusher interface {
    Flush() error
}

func writeAndFlush(w io.Writer, data []byte) error {
    if _, err := w.Write(data); err != nil {
        return err
    }

    // 检查 Writer 是否也实现了 Flusher
    if flusher, ok := w.(Flusher); ok {
        return flusher.Flush()
    }

    return nil
}
```

## 经 interface 做依赖注入

```go
package app

import "context"

// 为依赖定义 interface
type UserRepository interface {
    GetUser(ctx context.Context, id string) (*User, error)
    SaveUser(ctx context.Context, user *User) error
}

type EmailSender interface {
    SendEmail(ctx context.Context, to, subject, body string) error
}

// Service 依赖 interface
type UserService struct {
    repo   UserRepository
    mailer EmailSender
}

func NewUserService(repo UserRepository, mailer EmailSender) *UserService {
    return &UserService{
        repo:   repo,
        mailer: mailer,
    }
}

func (s *UserService) RegisterUser(ctx context.Context, email string) error {
    user := &User{Email: email}
    if err := s.repo.SaveUser(ctx, user); err != nil {
        return err
    }
    return s.mailer.SendEmail(ctx, email, "Welcome", "Thanks for registering!")
}

// 测试里好 mock
type MockUserRepository struct{}

func (m *MockUserRepository) GetUser(ctx context.Context, id string) (*User, error) {
    return &User{ID: id}, nil
}

func (m *MockUserRepository) SaveUser(ctx context.Context, user *User) error {
    return nil
}
```

## 速查表

| 模式               | 适用场景   | 核心原则             |
| ------------------ | ---------- | -------------------- |
| 小 interface       | 灵活性     | 单方法 interface     |
| 收 interface       | 可测试性   | 依赖抽象             |
| 返回 struct        | 清晰       | 具体返回类型         |
| io.Reader/Writer   | I/O 操作   | 标准库集成           |
| 内嵌               | 组合       | 不用继承也能扩展行为 |
| functional options | 配置       | 灵活的构造函数       |
| 类型断言           | 运行时检查 | 安全的向下转型       |
