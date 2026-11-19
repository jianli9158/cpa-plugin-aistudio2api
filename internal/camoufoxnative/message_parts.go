package camoufoxnative

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// FunctionCallMarker 是 GenerateContent 响应流中函数调用 Part 的开头：十个空字段后为带函数名的 functionCall
var FunctionCallMarker = []byte(`[null,null,null,null,null,null,null,null,null,null,["`)

// generateContents 返回 GenerateContent 正文的 contents
func generateContents(body []byte) ([]json.RawMessage, error) {
	var fields []json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || len(fields) < 2 {
		return nil, fmt.Errorf("GenerateContent 正文缺少 contents: %v", err)
	}
	var contents []json.RawMessage
	if err := json.Unmarshal(fields[1], &contents); err != nil {
		return nil, fmt.Errorf("解析 GenerateContent contents: %w", err)
	}
	return contents, nil
}

// contentRole 返回 wire 轮次 [parts, role] 的角色
func contentRole(content json.RawMessage) string {
	var fields []json.RawMessage
	var role string
	if json.Unmarshal(content, &fields) == nil && len(fields) > 1 {
		_ = json.Unmarshal(fields[1], &role)
	}
	return role
}

// contentText 返回 wire 轮次各 Part 的非空文本以空格连接的结果
func contentText(content json.RawMessage) string {
	var fields []json.RawMessage
	var parts [][]json.RawMessage
	if json.Unmarshal(content, &fields) != nil || len(fields) == 0 || json.Unmarshal(fields[0], &parts) != nil {
		return ""
	}
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		var text string
		if len(part) > 1 && json.Unmarshal(part[1], &text) == nil && text != "" {
			values = append(values, text)
		}
	}
	return strings.Join(values, " ")
}

// GeneratePrompt 返回 GenerateContent 正文最后一个轮次的文本，即官网用户在输入框中输入的提示词；最后一个轮次不是带文本的 user 轮次时返回 false
func GeneratePrompt(body []byte) (string, bool) {
	contents, err := generateContents(body)
	if err != nil || len(contents) == 0 || contentRole(contents[len(contents)-1]) != "user" {
		return "", false
	}
	text := contentText(contents[len(contents)-1])
	return text, text != ""
}

// lastUserParts 返回 GenerateContent 正文最后一个轮次是 user 轮次时它的各 Part，否则为 nil
func lastUserParts(body []byte) [][]json.RawMessage {
	contents, err := generateContents(body)
	if err != nil || len(contents) == 0 || contentRole(contents[len(contents)-1]) != "user" {
		return nil
	}
	var turn []json.RawMessage
	var parts [][]json.RawMessage
	if json.Unmarshal(contents[len(contents)-1], &turn) != nil || len(turn) == 0 || json.Unmarshal(turn[0], &parts) != nil {
		return nil
	}
	return parts
}

// GenerateAttachments 返回 GenerateContent 正文最后一个 user 轮次中内联数据、Drive 文件与外部媒体 Part 的个数，即官网用户在输入框中加入的附件数
func GenerateAttachments(body []byte) int {
	count := 0
	for _, part := range lastUserParts(body) {
		for _, index := range []int{2, 5, 6} {
			if len(part) > index && !bytes.Equal(part[index], []byte("null")) {
				count++
				break
			}
		}
	}
	return count
}

// GenerateFiles 返回 GenerateContent 正文最后一个 user 轮次中各 Drive 文件 Part 的文件 ID，顺序与正文相同
func GenerateFiles(body []byte) []string {
	var files []string
	for _, part := range lastUserParts(body) {
		var file []string
		if len(part) > 5 && json.Unmarshal(part[5], &file) == nil && len(file) > 0 {
			files = append(files, file[0])
		}
	}
	return files
}

// chatAction 是一轮界面对话在标签页上的操作
type chatAction int

const (
	// chatContinue 在页面当前对话后输入提示词并点击 Run
	chatContinue chatAction = iota
	// chatNew 先点 New chat 并丢弃当前对话，再输入提示词并点击 Run
	chatNew
	// chatRerun 点击最后一轮的 Rerun
	chatRerun
)

// nextChatAction 返回页面上一次发出的对话 previous 之后要发出 contents 时的操作
func nextChatAction(previous, contents []json.RawMessage) chatAction {
	if len(previous) == 0 {
		return chatNew
	}
	equal := func(left, right []json.RawMessage) bool {
		if len(left) != len(right) {
			return false
		}
		for index := range left {
			if !bytes.Equal(left[index], right[index]) {
				return false
			}
		}
		return true
	}
	if equal(previous, contents) {
		return chatRerun
	}
	if len(contents) == len(previous)+2 && equal(previous, contents[:len(previous)]) && contentRole(contents[len(previous)]) == "model" {
		return chatContinue
	}
	return chatNew
}

// StartsNewChat 返回页面上一次发出的对话 previous 之后要发出 contents 时是否先开新对话
func StartsNewChat(previous, contents []json.RawMessage) bool {
	return nextChatAction(previous, contents) == chatNew
}
