package handler

import "strings"

// ifNoneMatch 判断客户端的 If-None-Match 是否命中我们发出的 ETag。
//
// 为什么不能直接字符串比较：
//   - nginx 的 gzip 模块在压缩响应时会把强 ETag 改写成弱校验形式 W/"..."，
//     客户端下一次就带着 W/ 前缀回来。精确比较会让 304 永远不成立，
//     等于把压缩省下的带宽用"每次都回传完整正文"再还回去。
//   - 按 RFC 语义 If-None-Match 可以是逗号分隔的列表，也可以直接是 *。
//
// 仍然保持强比较语义：W/"a" 与 "a" 视为同一份资源（同一份生成结果），
// 因为这里的 ETag 表示"数据指纹相同"，不做字节级强校验。
func ifNoneMatch(header string, etags ...string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if candidate == "*" {
			return true
		}
		candidate = strings.TrimPrefix(candidate, "W/")
		for _, etag := range etags {
			if candidate == etag {
				return true
			}
		}
	}
	return false
}
