package ot

import (
	"fmt"
	"testing"
)

// TestApply 测试基本的操作应用
func TestApply(t *testing.T) {
	// "hello world" 的rune分布: h(0),e(1),l(2),l(3),o(4),' '(5),w(6),o(7),r(8),l(9),d(10)
	doc := "hello world"

	tests := []struct {
		name     string
		op       Operation
		expected string
	}{
		{"insert at start", InsertOp(0, "Hi "), "Hi hello world"},
		{"insert at end", InsertOp(11, "!"), "hello world!"},
		{"delete space", DeleteOp(5, 1), "helloworld"},
		{"delete from start", DeleteOp(0, 6), "world"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Apply(doc, tc.op)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tc.expected {
				t.Errorf("got %q, want %q", result, tc.expected)
			}
		})
	}
}

// TestApplyUnicode 测试Unicode字符
func TestApplyUnicode(t *testing.T) {
	result, err := Apply("你好世界", InsertOp(2, "美丽"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "你好美丽世界" {
		t.Errorf("got %q, want %q", result, "你好美丽世界")
	}

	result, err = Apply("你好美丽世界", DeleteOp(2, 2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "你好世界" {
		t.Errorf("got %q, want %q", result, "你好世界")
	}
}

// TestNoop 测试空操作处理
func TestNoop(t *testing.T) {
	// 长度为0的delete应该被当作noop处理
	noop := DeleteOp(2, 0)
	if !noop.IsNoop() {
		t.Error("expected IsNoop to be true")
	}

	doc := "abc"
	result, err := Apply(doc, noop)
	if err != nil {
		t.Errorf("noop should not error: %v", err)
	}
	if result != doc {
		t.Errorf("noop should leave doc unchanged, got %q", result)
	}
}

// checkTransform 验证Transform的核心性质:
// Apply(Apply(doc, op1), op2') == Apply(Apply(doc, op2), op1')
func checkTransform(t *testing.T, doc string, op1, op2 Operation) {
	t.Helper()
	op1t, op2t := Transform(op1, op2)

	left, err1 := ApplyOps(doc, []Operation{op1, op2t})
	right, err2 := ApplyOps(doc, []Operation{op2, op1t})

	if err1 != nil || err2 != nil {
		t.Logf("doc=%q op1=%s op2=%s", doc, op1, op2)
		t.Logf("op1'=%s op2'=%s", op1t, op2t)
		t.Errorf("Apply error: %v / %v", err1, err2)
		return
	}

	if left != right {
		t.Logf("doc=%q op1=%s op2=%s", doc, op1, op2)
		t.Logf("op1'=%s op2'=%s", op1t, op2t)
		t.Errorf("CONSISTENCY VIOLATION: left=%q, right=%q", left, right)
	}
}

// TestTransformInsertInsert 测试两个insert的转换
func TestTransformInsertInsert(t *testing.T) {
	doc := "abc"
	cases := []struct{ op1, op2 Operation }{
		{InsertOp(1, "X"), InsertOp(2, "Y")},
		{InsertOp(2, "Y"), InsertOp(1, "X")},
		{InsertOp(1, "X"), InsertOp(1, "Y")},
		{InsertOp(0, "S"), InsertOp(3, "E")},
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("case%d", i), func(t *testing.T) {
			checkTransform(t, doc, c.op1, c.op2)
		})
	}
}

// TestTransformInsertDelete 测试insert和delete的转换
func TestTransformInsertDelete(t *testing.T) {
	doc := "hello world"
	cases := []struct{ op1, op2 Operation }{
		// insert在delete之前
		{InsertOp(0, "!"), DeleteOp(5, 1)},
		// delete在insert之前
		{DeleteOp(0, 5), InsertOp(10, "!")},
		// insert在delete范围内
		{InsertOp(7, "X"), DeleteOp(5, 6)},
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("case%d", i), func(t *testing.T) {
			checkTransform(t, doc, c.op1, c.op2)
		})
	}
}

// TestTransformDeleteInsert 测试delete和insert的转换
func TestTransformDeleteInsert(t *testing.T) {
	doc := "hello world"
	cases := []struct{ op1, op2 Operation }{
		// insert在delete之前
		{DeleteOp(5, 1), InsertOp(0, "!")},
		// delete在insert之前
		{InsertOp(10, "!"), DeleteOp(0, 5)},
		// insert在delete范围内
		{DeleteOp(5, 6), InsertOp(7, "X")},
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("case%d", i), func(t *testing.T) {
			checkTransform(t, doc, c.op1, c.op2)
		})
	}
}

// TestTransformDeleteDelete 测试两个delete的转换
func TestTransformDeleteDelete(t *testing.T) {
	doc := "abcdef"
	cases := []struct{ op1, op2 Operation }{
		{DeleteOp(0, 2), DeleteOp(4, 2)},   // disjoint
		{DeleteOp(1, 3), DeleteOp(2, 2)},   // op1 contains op2
		{DeleteOp(0, 6), DeleteOp(0, 6)},   // identical
		{DeleteOp(1, 3), DeleteOp(2, 3)},   // partial overlap
		{DeleteOp(2, 2), DeleteOp(1, 3)},   // op2 contains op1
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("case%d", i), func(t *testing.T) {
			checkTransform(t, doc, c.op1, c.op2)
		})
	}
}

// TestAdjustAgainstSeq 测试服务器端实际使用的 Adjust 函数
func TestAdjustAgainstSeq(t *testing.T) {
	doc := "abc"

	// 服务器已经执行了两个insert
	seq := []Operation{
		InsertOp(1, "X"), // "aXbc"
		InsertOp(2, "Y"), // "aXYbc"
	}

	// 客户端发送 Insert(1, "Z"), base_version=0
	clientOp := InsertOp(1, "Z")

	// 服务器需要 Adjust against seq
	adjusted := AdjustOpAgainstSeq(clientOp, seq)

	// 执行 adjusted op
	result, err := ApplyOps(doc, append(seq, adjusted))
	if err != nil {
		t.Fatalf("ApplyOps error: %v", err)
	}
	t.Logf("seq=%v, clientOp=%s, adjusted=%s, result=%q", seq, clientOp, adjusted, result)

	// 手动检查: "aXYbc" 基础上 Insert(2, "Z")? 不对
	// Adjust(Insert(1,"Z"), Insert(1,"X")): pos相等, baseOp优先 => Insert(2,"Z")
	// Adjust(Insert(2,"Z"), Insert(2,"Y")): pos相等, baseOp优先 => Insert(3,"Z")
	// 最终 adjusted = Insert(3,"Z")
	// "aXYbc" + Insert(3,"Z") = "aXYZbc"
	if result != "aXYZbc" {
		t.Errorf("expected 'aXYZbc', got %q", result)
	}
}

// TestAdjustWithDelete 测试 Adjust 中 delete 相关场景
func TestAdjustWithDelete(t *testing.T) {
	doc := "hello world"

	// 场景: delete 后 insert
	delOp := DeleteOp(5, 6)
	insOp := InsertOp(7, "X")

	// Adjust insert against delete
	adjusted := Adjust(insOp, delOp)
	t.Logf("Adjust(%s, %s) = %s", insOp, delOp, adjusted)

	// 顺序1: 先 delete 再 adjusted insert
	left, _ := Apply(doc, delOp)
	left, _ = Apply(left, adjusted)

	// 顺序2: 先 insert 再 delete (原始op)
	right, _ := Apply(doc, insOp)
	right, _ = Apply(right, delOp)

	t.Logf("顺序1 (delete then adjusted insert): %q", left)
	t.Logf("顺序2 (insert then delete): %q", right)
}
