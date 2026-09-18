package config

import (
	"fmt"
	"log"
	"os"

	"go.yaml.in/yaml/v2"
)

const ConfigPath = "config.yaml"
const defaultPort = 8080

// Config 保存程序运行时需要的全部配置。
// 现在只有 HTTP 配置；以后接入 MySQL、Redis、Kafka 时，再往这里加字段。
type Config struct {
	HTTP     HTTPConfig
	HTTPPort int
}

// HTTPConfig 是 HTTP 服务的配置。
type HTTPConfig struct {
	// Addr 是服务监听的地址，例如 ":8080"。
	Addr string
}

// Load 负责读取配置。
func Load() (*Config, error) {
	data, err := os.ReadFile(ConfigPath)

	var cfg Config
	if err != nil {
		log.Print("读取配置文件失败，使用默认值")
		cfg.HTTPPort = defaultPort
	}
	//当配置文件不存在时，将默认值作为配置输入
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		cfg.HTTPPort = defaultPort
	}

	cfg.HTTP.Addr = ":" + fmt.Sprintf("%d", cfg.HTTPPort)

	return &cfg, nil
}
