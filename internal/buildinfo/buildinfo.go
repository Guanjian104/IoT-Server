// Package buildinfo 保存构建期注入的版本。
// 默认值用于未通过 -ldflags 注入的本地运行。
package buildinfo

var (
	Version  = "dev"
	Revision = "none"
)
