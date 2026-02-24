# YourWayHome
一个帮你快速回家的小工具 A tool can help you go back to your private network

`YourWayHome` 是一个将 `frp client` 与 `gost` 处理链结合的 Go 程序。  
它在 frp 的 work connection 回调中接管 TCP 连接，并交给 gost service 处理。

## 功能说明

- 使用 frp 客户端连接 frps
- 在 `HandleWorkConnCb` 中接管连接
- 支持通过 `-H` 传入 gost 的 service 命令（格式与 `gost -L` 一致）

## 项目结构

- `main.go`：主程序入口
- `frpc.ini`：frp 客户端示例配置
- `Dockerfile`：容器镜像构建文件
- `.dockerignore`：Docker 构建忽略规则

## 本地运行

要求：

- Go `1.25+`

启动：

```bash
go run . -c ./frpc.ini -H "ss://"
```

或先编译再运行：

```bash
go build -o your-way-home .
./your-way-home -c ./frpc.ini -H "ss://"
```

## 命令行参数

- `-c, --config`：frpc 配置文件路径，默认 `./frpc.ini`
- `--strict_config`：严格解析配置，默认 `true`
- `--allow-unsafe`：允许的 unsafe 特性（可多次传入）
- `-H, --service`：gost service 命令，默认 `ss://`

示例：

```bash
./your-way-home -c ./frpc.ini -H "socks5://:1080"
```

## Docker 打包与运行

构建镜像：

```bash
docker build -t your-way-home:latest .
```

使用默认镜像内置配置运行：

```bash
docker run --rm your-way-home:latest
```

挂载本地配置运行：

```bash
docker run --rm \
  -v "$(pwd)/frpc.ini:/app/frpc.ini:ro" \
  your-way-home:latest
```

传入自定义 gost service：

```bash
docker run --rm \
  -v "$(pwd)/frpc.ini:/app/frpc.ini:ro" \
  your-way-home:latest \
  -c /app/frpc.ini -H "socks5://:1080"
```

## 常见问题

- `Cannot connect to the Docker daemon`  
  说明本机 Docker/OrbStack 未启动，先启动后再执行 `docker build`。
