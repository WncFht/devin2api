---
name: observability-first-debugging
description: 消除猜测与臆断的系统化调试方法论。加插桩收集能完整解释问题的具体数据。证据先于假设，观察先于方案。触发：'debugging' 'error investigation' 'why is this failing' 'unexpected behavior' 'test failures' 'non-zero exit codes' 'stack traces'。
version: 1.0.0
---

# Observability-First Debugging

别猜。加可观测性。搞清楚实际在发生什么。

## 核心原则

**先测量，再动手。**东西不好使时，解法几乎从来不是猜了再瞎试。解法是加插桩，产出能完整解释问题的那份具体信息。

## 问题

Agent（和开发者）会掉进「猜了再试」的陷阱：

- 试一下 → 不好使
- 猜一个修法 → 不好使
- 再瞎试一个 → 不好使
- 用户看着持续扑空开始烦躁

**为什么会这样：**数据不足。你不知道实际在发生什么，就是在黑着打枪。

## 解法

**让看不见的变得看得见。**加日志、print、断言或调试输出，让你看到：

- 变量实际装的值
- 实际在跑的代码路径
- 外部系统实际返回的东西
- 期望在哪里与现实分岔

## 调试规程

### 1. 复现并记录症状

**到底是什么在失败？**

- 确切错误信息（复制粘贴，别转述）
- 期望行为 vs 实际行为
- 最小复现步骤

**不要：**

- 猜错误「大概是什么意思」
- 从症状直接断定原因

### 2. 先加可观测性

**在形成任何假设之前，先给系统插桩：**

加日志/print 展示：

- 函数入口的输入值
- 中间计算结果
- 返回值
- 走了哪个条件分支
- 外部 API 响应
- 状态变化

**示例：**

```python
def process_request(data):
    print(f"[DEBUG] Received data: {data}")
    print(f"[DEBUG] Data type: {type(data)}")

    result = transform(data)
    print(f"[DEBUG] After transform: {result}")

    if validate(result):
        print(f"[DEBUG] Validation passed")
        return save(result)
    else:
        print(f"[DEBUG] Validation FAILED")
        print(f"[DEBUG] Validation errors: {get_validation_errors(result)}")
        return None
```

**目标：**产出能明确展示每一步在干什么的输出。

### 3. 跑并观察

带着插桩执行。抓输出。

**找：**

- 与期望不符的值
- 该跑没跑的代码路径
- 比可见症状更早发生的错误
- 该有数据的地方出现 null/undefined

### 4. 形成有证据的假设

**现在你有数据了：**

- 证据显示了什么？
- 现实在哪里与期望分岔？
- 事情开始出错最早的点在哪？

**你的假设必须：**

- 基于观察到的数据（不是臆断）
- 能解释全部症状
- 可检验

### 5. 检验假设

加定点插桩或做实验：

- 觉得变量 X 不对，就在每个赋值点打印它
- 觉得函数 Y 没被调，就加入口/出口日志
- 觉得数据结构畸形，就打印它的形状

### 6. 迭代

假设错了，插桩会告诉你为什么。加更多可观测性，重复。

## 要消灭的反模式

### ❌ 无数据臆断

「可能是竞态」
「可能是缓存问题」
「可能是 API 超时」

**修：**加能证实或证伪每个理论的日志。

### ❌ 随机改动

改代码指望能修好，却不理解为什么会坏。

**修：**先通过可观测性理解 bug，再修根因。

### ❌ 一次试好几样

同时改 3 处，于是不知道是哪处修好的（甚至不知道是否真修好了）。

**修：**一次改一处。每处都用插桩验证。

### ❌ 以为代码言行一致

「这个函数应该返回用户数据」≠ 它真的返回。

**修：**打印它实际返回的东西。验证你的假设。

## 按场景分的可观测性手段

### 命令行工具

```bash
set -x  # 执行前打印每条命令
command -v foo  # 检查命令是否存在
echo "Value: $VAR"  # 打印变量值
```

### 代码调试

- 在关键决策点 print
- 给不变量加断言
- 记录函数入口/出口
- 转储数据结构
- 在出错点打栈追踪

### API/网络问题

- 打印完整请求（URL、headers、body）
- 打印完整响应（status、headers、body）
- 打印超时值
- 记录重试尝试

### 文件操作

- 打印正在访问的文件路径
- 操作前检查文件存在性
- 读后打印文件内容
- 验证写入成功

### 环境问题

- 打印环境变量
- 打印工作目录
- 打印 PATH 与其他配置
- 打印工具版本信息

## 决策树

```
Problem occurs
    ↓
Can you see the exact failure point?
    NO → Add logging/prints to trace execution flow
    YES ↓
Do you know the input values at failure?
    NO → Print input values and parameters
    YES ↓
Do you know what the code is actually doing?
    NO → Print intermediate results, branches taken
    YES ↓
Do you know why it's doing the wrong thing?
    NO → Print state, compare to expected state
    YES ↓
Fix the bug
```

## 示例

### 示例 1：测试失败

**症状：**测试报 "Expected 3, got undefined"

**❌ 臆断：**
「可能 mock 没生效」
「可能是异步时序问题」
[瞎试各种修法]

**✅ 可观测性优先：**

```javascript
test("calculates total", () => {
    const items = [1, 2, 3];
    console.log("Input items:", items);

    const result = calculateTotal(items);
    console.log("Result:", result);
    console.log("Result type:", typeof result);

    expect(result).toBe(6);
});
```

**输出显示：** `Result: undefined`

**循证行动：**查 `calculateTotal` 实际返回什么。在函数内部加日志，看它在哪里没算出来/没返回。

### 示例 2：API 调用不好使

**症状：**API 返回 400

**❌ 臆断：**
「可能 endpoint 变了」
「可能 auth token 过期了」
[随机试不同 endpoint]

**✅ 可观测性优先：**

```python
url = f"{BASE_URL}/api/users"
headers = {"Authorization": f"Bearer {token}"}
payload = {"name": name, "email": email}

print(f"[DEBUG] URL: {url}")
print(f"[DEBUG] Headers: {headers}")
print(f"[DEBUG] Payload: {payload}")

response = requests.post(url, headers=headers, json=payload)

print(f"[DEBUG] Status: {response.status_code}")
print(f"[DEBUG] Response: {response.text}")
```

**输出显示：** `Response: {"error": "email field is required"}`

**循证行动：**payload 构造错了。查 `email` 变量在哪里赋值。

### 示例 3：文件找不到

**症状：**`FileNotFoundError: foo.txt`

**❌ 臆断：**
「可能路径写错了」
[随机试各种路径变体]

**✅ 可观测性优先：**

```python
import os

file_path = "foo.txt"
print(f"[DEBUG] Looking for: {file_path}")
print(f"[DEBUG] Current directory: {os.getcwd()}")
print(f"[DEBUG] Directory contents: {os.listdir('.')}")
print(f"[DEBUG] File exists: {os.path.exists(file_path)}")

if not os.path.exists(file_path):
    abs_path = os.path.abspath(file_path)
    print(f"[DEBUG] Absolute path would be: {abs_path}")
```

**输出显示：**当前目录是 `/app/src`，文件在 `/app/data`

**循证行动：**用正确路径 `../data/foo.txt`，或修工作目录。

## 与用户反馈的配合

用户说你走错路时：

1. **立刻停**
2. **问他们观察到了什么**才下此结论
3. **加插桩**验证他们的洞见
4. **看输出**，调整方向

用户懂自己的系统。他们提出简单/显然的解法时，通常是对的。别想太多。

## 记住

- 调试是科学，不是猜谜
- 证据先于假设
- 观察先于方案
- 简单插桩 > 复杂理论
- 听用户给的线索

**目标：**产出能完整解释问题的具体数据，然后修法不证自明。

---

资料来源：

- [A systematic approach to debugging](https://ntietz.com/blog/how-i-debug-2023/)
- [Observability-based Debugging Mindset](https://mohitkarekar.com/posts/2024/observability-debugging/)
- [MIT 6.031: Debugging](http://web.mit.edu/6.031/www/fa17/classes/13-debugging/)
