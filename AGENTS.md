## 智能插座智能充电系统

### 项目说明

这是个智能插座智能充电系统,目前只支持易微联开关

### 大纲结构

`docs`目录保存项目相关需求、变更、设计文档，有需求变动时应该在这里进行变更

### git仓库

* commit、tag 等操作的comment一律使用中文描述

### 测试与验证

这是一个简单项目，不维护自动化测试。不要新增测试文件，也不要在构建或发布流程中运行测试。

### 构建系统

项目统一使用 Go Task（`Taskfile.yml`）作为构建系统。不要绕过 Taskfile 直接运行 `gofmt`、`go vet` 或 `go build`；代码格式化使用 `task fmt`，静态检查使用 `task check`，本地构建使用 `task build`，发布打包使用 `task release VERSION=vX.Y.Z`。
