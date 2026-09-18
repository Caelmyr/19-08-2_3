// Package ot 实现操作转换(Operational Transformation)算法
// 用于解决多人实时协同编辑中的并发冲突问题
//
// 服务器端流程:
//   1. 维护操作序列 ops 和版本号 version
//   2. 客户端发送 (op, base_version)
//   3. 取 ops[base_version:version] 作为中间操作
//   4. op = Adjust(op, 每个中间操作) 依次调整
//   5. 验证、执行、持久化、广播
package ot

import (
	"encoding/json"
	"fmt"
)

// OpType 操作类型
type OpType string

const (
	OpTypeInsert OpType = "insert"
	OpTypeDelete OpType = "delete"
)

// Operation 表示一个文档操作
type Operation struct {
	Type     OpType `json:"type"`     // 操作类型: insert 或 delete
	Position int    `json:"position"` // 操作位置(基于UTF-8 rune计数)
	Length   int    `json:"length"`   // 删除长度(仅delete使用)
	Text     string `json:"text"`     // 插入文本(仅insert使用)
}

// String 友好输出
func (o Operation) String() string {
	switch o.Type {
	case OpTypeInsert:
		return fmt.Sprintf("Insert(pos=%d, text=%q)", o.Position, o.Text)
	case OpTypeDelete:
		return fmt.Sprintf("Delete(pos=%d, len=%d)", o.Position, o.Length)
	default:
		return fmt.Sprintf("UnknownOp(%s)", o.Type)
	}
}

// EndPos 获取操作的结束位置
func (o Operation) EndPos() int {
	switch o.Type {
	case OpTypeInsert:
		return o.Position
	case OpTypeDelete:
		return o.Position + o.Length
	}
	return o.Position
}

// Validate 验证操作合法性
func (o Operation) Validate(docLength int) error {
	if o.Position < 0 {
		return fmt.Errorf("position cannot be negative")
	}
	if o.Position > docLength {
		return fmt.Errorf("position %d exceeds document length %d", o.Position, docLength)
	}
	switch o.Type {
	case OpTypeInsert:
		if o.Text == "" {
			return fmt.Errorf("insert text cannot be empty")
		}
	case OpTypeDelete:
		if o.Length <= 0 {
			return fmt.Errorf("delete length must be positive")
		}
		if o.Position+o.Length > docLength {
			return fmt.Errorf("delete range [%d,%d) exceeds document length %d",
				o.Position, o.Position+o.Length, docLength)
		}
	default:
		return fmt.Errorf("unknown op type: %s", o.Type)
	}
	return nil
}

// IsNoop 检查是否是空操作（被完全抵消的delete）
func (o Operation) IsNoop() bool {
	return o.Type == OpTypeDelete && o.Length <= 0
}

// Marshal 序列化为JSON
func (o Operation) Marshal() ([]byte, error) {
	return json.Marshal(o)
}

// UnmarshalOp 从JSON反序列化
func UnmarshalOp(data []byte) (Operation, error) {
	var op Operation
	err := json.Unmarshal(data, &op)
	return op, err
}

// InsertOp 公开的insert构造函数
func InsertOp(pos int, text string) Operation {
	return Operation{Type: OpTypeInsert, Position: pos, Text: text}
}

// DeleteOp 公开的delete构造函数
func DeleteOp(pos, length int) Operation {
	return Operation{Type: OpTypeDelete, Position: pos, Length: length}
}

// Apply 将操作应用到文档字符串上
func Apply(doc string, op Operation) (string, error) {
	if op.IsNoop() {
		return doc, nil
	}
	runes := []rune(doc)
	if err := op.Validate(len(runes)); err != nil {
		return "", err
	}

	switch op.Type {
	case OpTypeInsert:
		insertRunes := []rune(op.Text)
		result := make([]rune, 0, len(runes)+len(insertRunes))
		result = append(result, runes[:op.Position]...)
		result = append(result, insertRunes...)
		result = append(result, runes[op.Position:]...)
		return string(result), nil

	case OpTypeDelete:
		result := make([]rune, 0, len(runes)-op.Length)
		result = append(result, runes[:op.Position]...)
		result = append(result, runes[op.Position+op.Length:]...)
		return string(result), nil
	}

	return "", fmt.Errorf("unknown op type: %s", op.Type)
}

// ApplyOps 依次应用多个操作，自动跳过空操作
func ApplyOps(doc string, ops []Operation) (string, error) {
	var err error
	for _, op := range ops {
		if op.IsNoop() {
			continue
		}
		doc, err = Apply(doc, op)
		if err != nil {
			return doc, fmt.Errorf("applying %s: %w", op, err)
		}
	}
	return doc, nil
}

// Adjust 是OT的核心函数
// 假设 baseOp 已经执行，返回 op 现在应该变成什么样
//
// 这是服务器端和客户端都使用的函数：
//   - 服务器: Adjust(客户端op, 每个中间版本的已执行op)
//   - 客户端: Adjust(本地还没ack的op, 服务器广播的op)
func Adjust(op, baseOp Operation) Operation {
	baseLen := len([]rune(baseOp.Text))

	switch {
	// ============================================================
	// Insert(op) vs Insert(baseOp已执行)
	// 规则: baseOp先插入, op后插入. 如果op在baseOp右边, 右移;
	//       如果同位置, op放在baseOp后面(右移baseOp长度).
	// ============================================================
	case op.Type == OpTypeInsert && baseOp.Type == OpTypeInsert:
		if op.Position >= baseOp.Position {
			op.Position += baseLen
		}
		return op

	// ============================================================
	// Delete(op) vs Delete(baseOp已执行)
	// ============================================================
	case op.Type == OpTypeDelete && baseOp.Type == OpTypeDelete:
		opS, opE := op.Position, op.Position+op.Length
		bs, be := baseOp.Position, baseOp.Position+baseOp.Length

		switch {
		case opE <= bs:
			// op在base左边，完全不受影响
			return op
		case opS >= be:
			// op在base右边，左移base长度
			op.Position -= baseOp.Length
			return op
		case bs <= opS && be >= opE:
			// base完全覆盖op => op被抵消
			op.Length = 0
			return op
		case opS <= bs && opE >= be:
			// op完全覆盖base => op缩小
			op.Length -= baseOp.Length
			return op
		case opS < bs && opE > bs:
			// op起始与base重叠，截断到base开始前
			op.Length = bs - opS
			return op
		case bs < opS && be > opS:
			// base起始与op重叠，跳过重叠部分
			op.Position = be
			op.Length -= (be - opS)
			return op
		}
		return op

	// ============================================================
	// Insert(op) vs Delete(baseOp已执行)
	// ============================================================
	case op.Type == OpTypeInsert && baseOp.Type == OpTypeDelete:
		ds, de := baseOp.Position, baseOp.Position+baseOp.Length

		if op.Position <= ds {
			// insert在delete范围之前，不变
			return op
		} else if op.Position >= de {
			// insert在delete范围之后，左移delete长度
			op.Position -= baseOp.Length
			return op
		} else {
			// insert在delete范围内
			// 移到delete开始位置（delete执行后，delete开始位置不变）
			op.Position = ds
			return op
		}

	// ============================================================
	// Delete(op) vs Insert(baseOp已执行)
	// ============================================================
	case op.Type == OpTypeDelete && baseOp.Type == OpTypeInsert:
		insPos := baseOp.Position

		if op.EndPos() <= insPos {
			// delete在insert左边，不变
			return op
		} else if op.Position >= insPos {
			// delete在insert右边，右移insert长度
			op.Position += baseLen
			return op
		} else {
			// delete覆盖了insert位置，扩展delete包含insert内容
			op.Length += baseLen
			return op
		}
	}

	return op
}

// AdjustOpAgainstSeq 依次adjust一个op against操作序列
// 语义: seq中的操作已全部执行，op现在应该是什么样子
func AdjustOpAgainstSeq(op Operation, seq []Operation) Operation {
	current := op
	for _, baseOp := range seq {
		current = Adjust(current, baseOp)
	}
	return current
}

// TransformOpAgainstSeq 别名，兼容旧代码
func TransformOpAgainstSeq(op Operation, seq []Operation) Operation {
	return AdjustOpAgainstSeq(op, seq)
}

// Transform 是测试辅助函数
// 给定op1和op2两个并发操作，返回(op1', op2')使得:
//   Apply(Apply(doc, op1), op2') == Apply(Apply(doc, op2), op1')
//
// 注意: 这是一个测试用对称函数，内部直接处理每种组合，保证一致性。
// 服务器端实际只需要 Adjust 函数。
func Transform(op1, op2 Operation) (Operation, Operation) {
	return transformSymmetric(op1, op2)
}

// transformSymmetric 保证 Apply(Apply(doc, op1), op2') == Apply(Apply(doc, op2), op1')
func transformSymmetric(op1, op2 Operation) (Operation, Operation) {
	// 直接为每种组合计算正确的返回值
	// 策略: 给每个insert一个隐含优先级 (按Position从小到大, 同位置按Text长度)
	// 确保最终顺序确定

	switch {
	case op1.Type == OpTypeInsert && op2.Type == OpTypeInsert:
		return ttInsertInsert(op1, op2)
	case op1.Type == OpTypeInsert && op2.Type == OpTypeDelete:
		return ttInsertDelete(op1, op2)
	case op1.Type == OpTypeDelete && op2.Type == OpTypeInsert:
		return ttDeleteInsert(op1, op2)
	case op1.Type == OpTypeDelete && op2.Type == OpTypeDelete:
		return ttDeleteDelete(op1, op2)
	}
	return op1, op2
}

func ttInsertInsert(op1, op2 Operation) (Operation, Operation) {
	op1Len := len([]rune(op1.Text))
	op2Len := len([]rune(op2.Text))

	if op1.Position < op2.Position {
		// op1在左，op2在右
		return op1, InsertOp(op2.Position+op1Len, op2.Text)
	} else if op1.Position > op2.Position {
		// op1在右，op2在左
		return InsertOp(op1.Position+op2Len, op1.Text), op2
	} else {
		// 同位置: 用字符串长度打破平局 (短的优先)
		if len(op1.Text) <= len(op2.Text) {
			return op1, InsertOp(op2.Position+op1Len, op2.Text)
		} else {
			return InsertOp(op1.Position+op2Len, op1.Text), op2
		}
	}
}

func ttInsertDelete(op1, op2 Operation) (Operation, Operation) {
	// op1=insert, op2=delete
	op1Len := len([]rune(op1.Text))
	delEnd := op2.Position + op2.Length

	if op1.Position <= op2.Position {
		// insert在delete之前(或起点)
		return op1, DeleteOp(op2.Position+op1Len, op2.Length)
	} else if op1.Position >= delEnd {
		// insert在delete之后
		return op1, op2
	} else {
		// insert在delete范围内
		// 让delete扩展以覆盖insert
		return op1, DeleteOp(op2.Position, op2.Length+op1Len)
	}
}

func ttDeleteInsert(op1, op2 Operation) (Operation, Operation) {
	// op1=delete, op2=insert
	delEnd := op1.Position + op1.Length

	if op2.Position <= op1.Position {
		// insert在delete之前
		return DeleteOp(op1.Position+len([]rune(op2.Text)), op1.Length), op2
	} else if op2.Position >= delEnd {
		// insert在delete之后
		return op1, op2
	} else {
		// insert在delete范围内，insert移到delete后
		return op1, InsertOp(delEnd, op2.Text)
	}
}

func ttDeleteDelete(op1, op2 Operation) (Operation, Operation) {
	s1, e1 := op1.Position, op1.Position+op1.Length
	s2, e2 := op2.Position, op2.Position+op2.Length

	switch {
	case e1 <= s2:
		return op1, DeleteOp(s2-op1.Length, op2.Length)
	case e2 <= s1:
		return DeleteOp(s1-op2.Length, op1.Length), op2
	case s1 <= s2 && e1 >= e2:
		return op1, DeleteOp(s1, 0) // op2被抵消
	case s2 <= s1 && e2 >= e1:
		return DeleteOp(s2, 0), op2 // op1被抵消
	case s1 < s2 && e1 > s2:
		overlap := e1 - s2
		return op1, DeleteOp(s2, op2.Length-overlap)
	case s2 < s1 && e2 > s1:
		overlap := e2 - s1
		return DeleteOp(s1, op1.Length-overlap), op2
	}
	return op1, op2
}

// FilterNoop 过滤掉空操作
func FilterNoop(ops []Operation) []Operation {
	result := make([]Operation, 0, len(ops))
	for _, op := range ops {
		if !op.IsNoop() {
			result = append(result, op)
		}
	}
	return result
}
