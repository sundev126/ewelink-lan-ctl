# README 简体中文翻译实施计划

> **供智能代理执行：** 必须使用 `superpowers:subagent-driven-development`（推荐）或 `superpowers:executing-plans`，逐项实施本计划。步骤使用复选框（`- [ ]`）跟踪。

**目标：** 将根目录 `README.md` 的说明文字完整翻译为简体中文，同时保持所有技术标识和可执行示例不变。

**实现方式：** 仅修改一个 Markdown 文件。保持现有章节结构、警告、表格、列表和代码块；翻译自然语言，随后通过差异审查验证命令、变量、路径、JSON 字段、状态码和阈值没有变化。

**技术栈：** Markdown、Git、ripgrep。

## 全局约束

- spec 和 plan 使用中文书写。
- 项目名、OAuth、REST、JSON、Docker Compose、systemd、HTTP、API、LAN、VPN、App Secret 和 device ID 等技术术语保持原样。
- 命令、文件名、环境变量、URL、API 路径、HTTP 方法、JSON 字段、错误码和关键数值保持不变。
- 不改变无认证安全模型、部署行为、OAuth 流程或快捷指令逻辑。

---

### Task 1：翻译并校验 README

**文件：**

- 修改：`README.md`
- 参照：`docs/superpowers/specs/2026-07-16-readme-chinese-translation-design.md`

**接口：**

- 输入：当前英文 README 的章节、命令和示例。
- 输出：结构相同、技术内容不变的简体中文 README。

- [ ] **步骤 1：保存需要保持不变的技术标识清单**

  从 README 提取代码块以及环境变量、API 路径和错误码，用于翻译后的人工差异核对。重点包括 `EWELINK_*`、`/oauth/start`、`/callback`、`/healthz`、`/api/v1/*`、`on`、`off` 和所有稳定错误码。

- [ ] **步骤 2：翻译自然语言内容**

  将标题、段落、警告、表格说明、操作步骤和代码注释翻译为简体中文；不修改代码块中的命令、URL、JSON 结构和示例值，仅允许翻译 `#` 后的自然语言注释。

- [ ] **步骤 3：检查未翻译的英文说明**

  运行：

  ```bash
  rg -n '^(##?|> |[0-9]+\. |[A-Z][A-Za-z]+ )' README.md
  ```

  预期：只剩项目名、技术术语、命令或确有必要保留的英文标识；没有完整英文说明段落。

- [ ] **步骤 4：检查 Markdown 和技术内容**

  运行：

  ```bash
  git diff --check
  git diff -- README.md
  ```

  预期：`git diff --check` 返回 0；差异仅为自然语言翻译，命令、环境变量、路径、JSON 字段、状态码及 20%/80% 阈值没有变化。

- [ ] **步骤 5：提交翻译**

  ```bash
  git add README.md
  git commit -m "docs: translate readme into Chinese"
  ```

  预期：提交成功，工作区没有 README 的未提交改动。
