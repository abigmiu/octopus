package relay

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/bestruirui/octopus/internal/conf"
	"github.com/charmbracelet/log"
)

// bodyDir 是请求体与响应体的落盘目录; InitBodyStore 依据数据库路径所在的 data 目录推导。
// 进程内请求 ID 每次启动都会从头计数, 而内存中的请求元数据也在启动时清空, 故启动时清空整目录,
// 避免上一次运行遗留的孤儿文件与本轮重号请求相互混淆, 也让目录大小与当前运行期存活的请求一致。
var bodyDir string

// InitBodyStore 计算落盘目录并在启动时清空残留文件, 调用方需在配置加载完成后执行一次。
func InitBodyStore() {
	bodyDir = filepath.Join(filepath.Dir(conf.AppConfig.Database.Path), "request_body")
	if err := os.RemoveAll(bodyDir); err != nil {
		log.Warnf("failed to clean request body dir %q: %v", bodyDir, err)
	}
	if err := os.MkdirAll(bodyDir, 0o755); err != nil {
		log.Warnf("failed to create request body dir %q: %v", bodyDir, err)
	}
}

// requestBodyFilePath 返回请求体文件路径。
func requestBodyFilePath(id uint64) string {
	return filepath.Join(bodyDir, strconv.FormatUint(id, 10)+".req")
}

// responseBodyFilePath 返回响应体文件路径。
func responseBodyFilePath(id uint64) string {
	return filepath.Join(bodyDir, strconv.FormatUint(id, 10)+".resp")
}

// saveRequestBody 将请求体写入落盘目录, 默认在请求到达时调用一次。
// 存储目录尚未初始化时静默跳过, 避免写入当前工作目录。
func saveRequestBody(id uint64, body string) {
	if bodyDir == "" || body == "" {
		return
	}
	if err := os.WriteFile(requestBodyFilePath(id), []byte(body), 0o644); err != nil {
		log.Warnf("failed to persist request body for request %d: %v", id, err)
	}
}

// saveResponseBody 将最终响应体写入落盘目录; 空响应体不落盘。
func saveResponseBody(id uint64, body string) {
	if bodyDir == "" || body == "" {
		return
	}
	if err := os.WriteFile(responseBodyFilePath(id), []byte(body), 0o644); err != nil {
		log.Warnf("failed to persist response body for request %d: %v", id, err)
	}
}

// readRequestBody 读取指定请求的请求体, 文件不存在或读取失败均返回空串。
func readRequestBody(id uint64) string {
	return readBodyFile(requestBodyFilePath(id))
}

// readResponseBody 读取指定请求的响应体, 文件不存在或读取失败均返回空串。
func readResponseBody(id uint64) string {
	return readBodyFile(responseBodyFilePath(id))
}

// readBodyFile 整体读取一个落盘文件并转成字符串。
func readBodyFile(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(content)
}

// deleteBodyFiles 删除指定请求的请求体与响应体文件, 用于该请求被裁剪时清理。
func deleteBodyFiles(id uint64) {
	_ = os.Remove(requestBodyFilePath(id))
	_ = os.Remove(responseBodyFilePath(id))
}
