# Docs Guard——docstring、PHPDoc 与 JSDoc 规则

代码内文档相比其他文档面多一条约束：它就坐在事实旁边。docstring 与下面三行的签名不一致，没有任何借口。

## 目录

- 什么时候该写 docstring
- 复述测试
- 好 docstring 含什么
- 标签准确性（PHPDoc/JSDoc）
- 生成文档的卫生

## 什么时候该写 docstring

- 公开 API 面：永远——它喂 IDE、生成的参考文档和 agent。
- 内部辅助函数：只在契约无法由签名表达时写（单位、不变量、副作用、「为什么」）。名字已经表意的一行内部函数通常什么都不需要——且适用 clean-code-guard 的注释规则。

## 复述测试

删掉信息内容可以完整从签名恢复的 docstring：

```php
// 不通过——复述显而易见的事，什么都没文档化。
/**
 * Gets the user by ID.
 *
 * @param int $user_id The user ID.
 * @return WP_User The user.
 */
function get_user_by_id( $user_id ) { /* … */ }
```

AI 生成器成千地吐这种东西：穿着西装的注释污染。要么说签名说不出的，要么什么都不说。

## 好 docstring 含什么

类型表达不了的契约：

- 单位与取值范围（`$timeout` 是秒还是毫秒？为 0 时怎样？）
- 错误行为：失败时抛什么/返回什么、何时抛（核验抛出/返回处）
- 副作用：写入、缓存失效、触发的事件、碰过的全局状态
- null/空语义：`null` 在这里意味着什么、空数组做什么
- 顺序、幂等、并发保证——当调用方依赖它们时
- 令人意外的设计写「为什么」（「API 失败时返回 1.0，价格就永远不会消失」）

## 标签准确性（PHPDoc/JSDoc）

- `@param` 的名字和顺序与签名完全一致——这里漂移是在对 IDE 撒谎。
- `@param`、`@return` 类型与真实类型一致，含可空（`int|WP_Error`、`?string`）和项目用到的泛型。
- `@throws` 列函数体实际抛的——逐个核验抛出处；不再抛的删掉。
- `@since` 与项目版本化的 changelog/tag 对应。
- `@deprecated` 必须指明替代。

## 生成文档的卫生

当 docstring 喂给生成的参考文档（phpDocumentor、JSDoc、Sphinx、TypeDoc）时：

- 错的 docstring 会变成发布出去的错的参考页——按 README 同级的规则 1 严重度处理。
- 检查 docstring 里的示例遵守 [code-samples.md](code-samples.md)——它们是任何代码库里最少被 review 的示例。
- 标记对所用生成器必须合法；坏标签会悄悄截断发布的页面。
