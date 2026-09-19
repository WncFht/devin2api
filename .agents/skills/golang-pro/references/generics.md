# Generics 与类型参数

## 基本类型参数

```go
package main

// 带类型参数的泛型函数
func Max[T constraints.Ordered](a, b T) T {
    if a > b {
        return a
    }
    return b
}

// 多个类型参数
func Map[T, U any](slice []T, fn func(T) U) []U {
    result := make([]U, len(slice))
    for i, v := range slice {
        result[i] = fn(v)
    }
    return result
}

// 用法
func main() {
    maxInt := Max(10, 20)           // T = int
    maxFloat := Max(3.14, 2.71)     // T = float64
    maxString := Max("abc", "xyz")  // T = string

    nums := []int{1, 2, 3}
    doubled := Map(nums, func(n int) int { return n * 2 })
    strings := Map(nums, func(n int) string { return fmt.Sprintf("%d", n) })
}
```

## 类型约束

```go
import "constraints"

// 内置约束
type Number interface {
    constraints.Integer | constraints.Float
}

func Sum[T Number](numbers []T) T {
    var total T
    for _, n := range numbers {
        total += n
    }
    return total
}

// 带方法的自定义约束
type Stringer interface {
    String() string
}

func PrintAll[T Stringer](items []T) {
    for _, item := range items {
        fmt.Println(item.String())
    }
}

// 用 ~ 表示的近似约束
type Integer interface {
    ~int | ~int8 | ~int16 | ~int32 | ~int64
}

type MyInt int

func Double[T Integer](n T) T {
    return n * 2
}

// int 和 MyInt 都能用
func main() {
    fmt.Println(Double(5))          // int
    fmt.Println(Double(MyInt(5)))   // MyInt
}
```

## 泛型数据结构

```go
// 泛型栈
type Stack[T any] struct {
    items []T
}

func NewStack[T any]() *Stack[T] {
    return &Stack[T]{
        items: make([]T, 0),
    }
}

func (s *Stack[T]) Push(item T) {
    s.items = append(s.items, item)
}

func (s *Stack[T]) Pop() (T, bool) {
    if len(s.items) == 0 {
        var zero T
        return zero, false
    }
    item := s.items[len(s.items)-1]
    s.items = s.items[:len(s.items)-1]
    return item, true
}

func (s *Stack[T]) IsEmpty() bool {
    return len(s.items) == 0
}

// 用法
intStack := NewStack[int]()
intStack.Push(1)
intStack.Push(2)

stringStack := NewStack[string]()
stringStack.Push("hello")
stringStack.Push("world")
```

## 泛型 map 操作

```go
// 泛型版 filter
func Filter[T any](slice []T, predicate func(T) bool) []T {
    result := make([]T, 0, len(slice))
    for _, v := range slice {
        if predicate(v) {
            result = append(result, v)
        }
    }
    return result
}

// reduce/fold
func Reduce[T, U any](slice []T, initial U, fn func(U, T) U) U {
    acc := initial
    for _, v := range slice {
        acc = fn(acc, v)
    }
    return acc
}

// 取 map 的键
func Keys[K comparable, V any](m map[K]V) []K {
    keys := make([]K, 0, len(m))
    for k := range m {
        keys = append(keys, k)
    }
    return keys
}

// 取 map 的值
func Values[K comparable, V any](m map[K]V) []V {
    values := make([]V, 0, len(m))
    for _, v := range m {
        values = append(values, v)
    }
    return values
}

// 用法
numbers := []int{1, 2, 3, 4, 5, 6}
evens := Filter(numbers, func(n int) bool { return n%2 == 0 })

sum := Reduce(numbers, 0, func(acc, n int) int { return acc + n })

m := map[string]int{"a": 1, "b": 2}
keys := Keys(m)     // []string{"a", "b"}
values := Values(m) // []int{1, 2}
```

## 泛型 Pair 与 Tuple

```go
// 泛型 pair
type Pair[T, U any] struct {
    First  T
    Second U
}

func NewPair[T, U any](first T, second U) Pair[T, U] {
    return Pair[T, U]{First: first, Second: second}
}

func (p Pair[T, U]) Swap() Pair[U, T] {
    return Pair[U, T]{First: p.Second, Second: p.First}
}

// 用法
pair := NewPair("name", 42)
swapped := pair.Swap() // Pair[int, string]

// 泛型 Result 类型（类似 Rust 的 Result<T, E>）
type Result[T any] struct {
    value T
    err   error
}

func Ok[T any](value T) Result[T] {
    return Result[T]{value: value}
}

func Err[T any](err error) Result[T] {
    return Result[T]{err: err}
}

func (r Result[T]) IsOk() bool {
    return r.err == nil
}

func (r Result[T]) Unwrap() (T, error) {
    return r.value, r.err
}

func (r Result[T]) UnwrapOr(defaultValue T) T {
    if r.err != nil {
        return defaultValue
    }
    return r.value
}
```

## comparable 约束

```go
// 用 comparable 实现查找
func Find[T comparable](slice []T, target T) (int, bool) {
    for i, v := range slice {
        if v == target {
            return i, true
        }
    }
    return -1, false
}

// 判断元素是否存在
func Contains[T comparable](slice []T, target T) bool {
    _, found := Find(slice, target)
    return found
}

// 元素去重
func Unique[T comparable](slice []T) []T {
    seen := make(map[T]struct{})
    result := make([]T, 0, len(slice))

    for _, v := range slice {
        if _, exists := seen[v]; !exists {
            seen[v] = struct{}{}
            result = append(result, v)
        }
    }

    return result
}

// 用法
nums := []int{1, 2, 2, 3, 3, 4}
unique := Unique(nums) // []int{1, 2, 3, 4}

idx, found := Find([]string{"a", "b", "c"}, "b") // 1, true
```

## 泛型 interface

```go
// 泛型 interface
type Container[T any] interface {
    Add(item T)
    Remove() (T, bool)
    Size() int
}

// 实现
type Queue[T any] struct {
    items []T
}

func (q *Queue[T]) Add(item T) {
    q.items = append(q.items, item)
}

func (q *Queue[T]) Remove() (T, bool) {
    if len(q.items) == 0 {
        var zero T
        return zero, false
    }
    item := q.items[0]
    q.items = q.items[1:]
    return item, true
}

func (q *Queue[T]) Size() int {
    return len(q.items)
}

// 接收泛型 interface 的函数
func ProcessContainer[T any](c Container[T], item T) {
    c.Add(item)
    fmt.Printf("Container size: %d\n", c.Size())
}
```

## 类型推断

```go
// 大多数情况下类型推断可用
func Identity[T any](x T) T {
    return x
}

// 无需指定类型
result := Identity(42)          // T 推断为 int
str := Identity("hello")        // T 推断为 string

// 带约束的类型推断
func Min[T constraints.Ordered](a, b T) T {
    if a < b {
        return a
    }
    return b
}

// 从实参推断
minVal := Min(10, 20)           // T = int
minFloat := Min(1.5, 2.5)       // T = float64

// 需要时显式指定类型
result := Map[int, string]([]int{1, 2}, func(n int) string {
    return fmt.Sprintf("%d", n)
})
```

## 泛型 channel

```go
// 泛型 channel 操作
func Merge[T any](channels ...<-chan T) <-chan T {
    out := make(chan T)
    var wg sync.WaitGroup

    for _, ch := range channels {
        wg.Add(1)
        go func(c <-chan T) {
            defer wg.Done()
            for v := range c {
                out <- v
            }
        }(ch)
    }

    go func() {
        wg.Wait()
        close(out)
    }()

    return out
}

// 泛型 pipeline 阶段
func Stage[T, U any](in <-chan T, fn func(T) U) <-chan U {
    out := make(chan U)
    go func() {
        defer close(out)
        for v := range in {
            out <- fn(v)
        }
    }()
    return out
}

// 用法
ch1 := make(chan int)
ch2 := make(chan int)

merged := Merge(ch1, ch2)

numbers := make(chan int)
doubled := Stage(numbers, func(n int) int { return n * 2 })
strings := Stage(doubled, func(n int) string { return fmt.Sprintf("%d", n) })
```

## 联合约束

```go
// 类型联合
type StringOrInt interface {
    string | int
}

func Process[T StringOrInt](val T) string {
    return fmt.Sprintf("%v", val)
}

// 更复杂的联合
type Numeric interface {
    int | int8 | int16 | int32 | int64 |
    uint | uint8 | uint16 | uint32 | uint64 |
    float32 | float64
}

func Abs[T Numeric](n T) T {
    if n < 0 {
        return -n
    }
    return n
}

// 带方法的联合
type Serializable interface {
    string | []byte
}

func Serialize[T Serializable](data T) []byte {
    switch v := any(data).(type) {
    case string:
        return []byte(v)
    case []byte:
        return v
    default:
        panic("unreachable")
    }
}
```

## 速查表

| 特性       | 语法                              | 适用场景                 |
| ---------- | --------------------------------- | ------------------------ |
| 泛型基础   | `func F[T any]()`                 | 任意类型                 |
| 约束       | `func F[T Constraint]()`          | 受限类型                 |
| 多参数     | `func F[T, U any]()`              | 多个类型变量             |
| comparable | `func F[T comparable]()`          | 支持 == 和 != 的类型     |
| ordered    | `func F[T constraints.Ordered]()` | 支持 <、>、<=、>= 的类型 |
| 联合       | `T interface{int \| string}`      | 二选一类型               |
| 近似       | `~int`                            | 包含类型别名             |
