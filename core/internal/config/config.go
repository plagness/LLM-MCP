package config

import (
	"os"
	"strconv"
)

// Getenv возвращает значение env-переменной или default
func Getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// GetenvInt возвращает int из env-переменной или default
func GetenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// HasOpenai проверяет наличие ключа OpenAI
func HasOpenai() bool {
	return os.Getenv("OPENAI_API_KEY") != ""
}

// HasOpenrouter проверяет наличие ключа OpenRouter
func HasOpenrouter() bool {
	return os.Getenv("OPENROUTER_API_KEY") != ""
}
