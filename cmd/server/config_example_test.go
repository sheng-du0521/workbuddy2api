package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// ======================================================================
// v0.26.7 · 配置样例完整性（防"加了配置项忘了同步示例"）
//
// 既有测试已经保证「config.example.json 能被 Load 解析」，但那是**单向**的：
// 代码里新增一个字段、示例里没写，解析照样成功 —— 于是示例悄悄变成过时的文档。
// 而 config.example.json 同时是**容器默认配置**（Dockerfile 把它 COPY 成
// /app/config.json），运维也照它写自己的配置：
//
//   - 示例里没有的键，运维不知道存在 → 该能力永远没人用；
//   - 更糟的是示例里的键**已失效**（代码删了字段但示例没删）→ 配了不生效，
//     且因为 JSON 未知键天然忽略，**不会有任何报错**。
//
// 所以这里做**反向检查**：结构体里每个可配置字段都必须在示例里出现。
// ======================================================================

// configExampleFieldExceptions 明确"有意不在示例里展示"的字段（路径小写）。
//
// 加白名单必须是**显式**的：默认要求展示，例外要写理由。
// 这样"忘了同步示例"与"有意省略"才区分得开 —— 否则白名单会慢慢变成垃圾桶。
var configExampleFieldExceptions = map[string]string{
	// value 是理由，仅作文档；测试不校验理由内容。
	"server": "已退役段（空结构体，其含义由注释说明，示例里保留该段的唯一价值是提示旧配置可安全保留）",
}

// TestConfigExampleCoversAllFields 每个结构体字段都必须在 config.example.json 里出现。
func TestConfigExampleCoversAllFields(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatalf("读示例配置失败: %v", err)
	}
	var example map[string]any
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatalf("示例配置不是合法 JSON: %v", err)
	}

	var missing []string
	walkConfigFields(reflect.TypeOf(Config{}), example, "", func(path string) {
		missing = append(missing, path)
	})

	// 过滤白名单。
	var real []string
	for _, p := range missing {
		if _, ok := configExampleFieldExceptions[p]; ok {
			continue
		}
		real = append(real, p)
	}
	if len(real) > 0 {
		t.Errorf("config.example.json 缺少以下字段（新增配置项必须同步示例，"+
			"否则运维不知道它存在、容器默认配置也不含它）：\n  %s\n"+
			"若确为有意省略，请加入 configExampleFieldExceptions 并写明理由。",
			strings.Join(real, "\n  "))
	}
}

// walkConfigFields 递归遍历类型 t 的字段，对每个带 json tag 的字段调用
// 检查：在示例 JSON 的 cur 子树里是否存在该键。不存在则回调 miss(path)。
//
// path 用小写点号（如 "upstream.timeout_seconds"），与白名单键一致。
func walkConfigFields(t reflect.Type, cur map[string]any, prefix string, miss func(string)) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		// 未导出字段不参与配置解析。
		if f.PkgPath != "" {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			// 没有显式 json tag：字段名即键名（Go 的默认行为），仍要检查。
			name = f.Name
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}

		child, ok := cur[name]
		if !ok {
			// 支持内嵌结构体（如 config.Schedule 作为匿名字段时无 tag）。
			miss(path)
			continue
		}

		// 递归：字段本身是结构体且示例里对应一个对象时，继续往下查。
		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			sub, isObj := child.(map[string]any)
			if !isObj {
				// 示例里该段不是对象（写错了类型）也算缺 —— 让 parse 去报更精确的错。
				continue
			}
			// time.Duration 之类底层为 int 的具名类型不会被当成 struct；
			// 真正的嵌套配置段才有字段可查。
			if ft.NumField() > 0 {
				walkConfigFields(ft, sub, path, miss)
			}
		}
	}
}

// TestConfigExampleNoStaleKeys 反向：示例里不该出现结构体没有的键。
//
// 与上面互补 —— 示例里多出来的键是"已失效配置"，配了不生效且**不报错**
// （JSON 未知键天然忽略），是最难被发现的一类。
func TestConfigExampleNoStaleKeys(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatalf("读示例配置失败: %v", err)
	}
	var example map[string]any
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatalf("示例配置不是合法 JSON: %v", err)
	}
	var stale []string
	walkConfigStale(reflect.TypeOf(Config{}), example, "", func(path string) {
		stale = append(stale, path)
	})
	if len(stale) > 0 {
		t.Errorf("config.example.json 含结构体里已不存在的配置键（配了不生效，且不会报错）：\n  %s",
			strings.Join(stale, "\n  "))
	}
}

// walkConfigStale 递归找出示例里有、类型里没有的键。
func walkConfigStale(t reflect.Type, cur map[string]any, prefix string, stale func(string)) {
	known := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = f.Name
		}
		known[name] = f.Type
	}
	for k, v := range cur {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		ft, ok := known[k]
		if !ok {
			stale(path)
			continue
		}
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if sub, isObj := v.(map[string]any); isObj && ft.Kind() == reflect.Struct && ft.NumField() > 0 {
			walkConfigStale(ft, sub, path, stale)
		}
	}
}
