package cursor

// Cursor 3.x agent.v1 协议为 protobuf(非 JSON)编码。本文件是 Qoder 号池原生实现的
// 最小 protobuf 线级编解码器(varint / length-delimited / fixed), 不依赖任何第三方或
// Cursor2api。仅覆盖 agent.v1 交互所需的线型。

import (
	"encoding/binary"
	"fmt"
)

// pbVarint 编码无符号变长整数
func pbVarint(v uint64) []byte {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], v)
	return append([]byte(nil), buf[:n]...)
}

// pbField 写入 num/wire 标签, 后接已编码 payload
func pbField(num int, wire byte, payload []byte) []byte {
	out := pbVarint(uint64(num)<<3 | uint64(wire))
	return append(out, payload...)
}

// pbBytes 写入 length-delimited(wire=2)字段
func pbBytes(num int, data []byte) []byte {
	out := pbField(num, 2, pbVarint(uint64(len(data))))
	return append(out, data...)
}

func pbString(num int, s string) []byte { return pbBytes(num, []byte(s)) }

// pbPart 一个已解析的 protobuf 字段
type pbPart struct {
	Num   int
	Wire  byte
	Data  []byte // wire 2/1/5 的原始字节
	Value uint64 // wire 0 (varint) 的值
}

// pbParse 解析一段 protobuf 消息为字段列表
func pbParse(data []byte) ([]pbPart, error) {
	var out []pbPart
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 {
			return nil, fmt.Errorf("invalid protobuf tag")
		}
		data = data[n:]
		num, wire := int(tag>>3), byte(tag&7)
		p := pbPart{Num: num, Wire: wire}
		switch wire {
		case 0: // varint
			v, m := binary.Uvarint(data)
			if m <= 0 {
				return nil, fmt.Errorf("invalid protobuf varint")
			}
			p.Value = v
			data = data[m:]
		case 2: // length-delimited
			l, m := binary.Uvarint(data)
			if m <= 0 || l > uint64(len(data[m:])) {
				return nil, fmt.Errorf("invalid protobuf bytes")
			}
			data = data[m:]
			p.Data = append([]byte(nil), data[:l]...)
			data = data[l:]
		case 1: // fixed64
			if len(data) < 8 {
				return nil, fmt.Errorf("invalid protobuf fixed64")
			}
			p.Data = append([]byte(nil), data[:8]...)
			data = data[8:]
		case 5: // fixed32
			if len(data) < 4 {
				return nil, fmt.Errorf("invalid protobuf fixed32")
			}
			p.Data = append([]byte(nil), data[:4]...)
			data = data[4:]
		default:
			return nil, fmt.Errorf("unsupported protobuf wire %d", wire)
		}
		out = append(out, p)
	}
	return out, nil
}

// pbFirst 返回首个匹配 num 的字段
func pbFirst(parts []pbPart, num int) (pbPart, bool) {
	for _, p := range parts {
		if p.Num == num {
			return p, true
		}
	}
	return pbPart{}, false
}

// ---- 高层编码助手(空值跳过, 契合 Cursor 请求消息构造) ----

func protoString(field int, value string) []byte {
	if value == "" {
		return nil
	}
	return pbBytes(field, []byte(value))
}

func protoMessage(field int, value []byte) []byte { return pbBytes(field, value) }

func protoBool(field int, value bool) []byte {
	if !value {
		return nil
	}
	out := pbVarint(uint64(field) << 3)
	return append(out, pbVarint(1)...)
}
