package main

import (
	"fmt"
	"coedit/internal/ot"
)

func main() {
	fmt.Println("=== Insert vs Insert 同位置 ===")
	doc := "abc"
	op1 := ot.InsertOp(1, "X")
	op2 := ot.InsertOp(1, "Y")
	op1t, op2t := ot.Transform(op1, op2)
	fmt.Printf("op1=%s, op2=%s\n", op1, op2)
	fmt.Printf("op1'=%s, op2'=%s\n", op1t, op2t)
	
	left, _ := ot.Apply(doc, op1)
	left, _ = ot.Apply(left, op2t)
	right, _ := ot.Apply(doc, op2)
	right, _ = ot.Apply(right, op1t)
	fmt.Printf("left=%q, right=%q\n", left, right)
	fmt.Println()

	fmt.Println("=== Insert in delete range ===")
	doc = "hello world"
	op1 = ot.InsertOp(7, "X")
	op2 = ot.DeleteOp(5, 6)
	op1t, op2t = ot.Transform(op1, op2)
	fmt.Printf("op1=%s, op2=%s\n", op1, op2)
	fmt.Printf("op1'=%s, op2'=%s\n", op1t, op2t)
	
	left2, err1 := ot.Apply(doc, op1)
	left2, err2 := ot.Apply(left2, op2t)
	right2, err3 := ot.Apply(doc, op2)
	right2, err4 := ot.Apply(right2, op1t)
	fmt.Printf("left=%q(err1=%v,err2=%v), right=%q(err3=%v,err4=%v)\n", left2, err1, err2, right2, err3, err4)
	fmt.Println()
	
	fmt.Println("=== Delete contains delete ===")
	doc = "abcdef"
	op1 = ot.DeleteOp(1, 3)
	op2 = ot.DeleteOp(2, 2)
	op1t, op2t = ot.Transform(op1, op2)
	fmt.Printf("op1=%s, op2=%s\n", op1, op2)
	fmt.Printf("op1'=%s, op2'=%s\n", op1t, op2t)
}
