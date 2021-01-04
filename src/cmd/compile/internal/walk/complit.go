// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package walk

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/ssagen"
	"cmd/compile/internal/staticdata"
	"cmd/compile/internal/staticinit"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
)

// walkCompLit walks a composite literal node:
// OARRAYLIT, OSLICELIT, OMAPLIT, OSTRUCTLIT (all CompLitExpr), or OPTRLIT (AddrExpr).
func walkCompLit(n ir.Node, init *ir.Nodes) ir.Node {
	if isStaticCompositeLiteral(n) && !ssagen.TypeOK(n.Type()) {
		n := n.(*ir.CompLitExpr) // not OPTRLIT
		// n can be directly represented in the read-only data section.
		// Make direct reference to the static data. See issue 12841.
		vstat := readonlystaticname(n.Type())
		fixedlit(inInitFunction, initKindStatic, n, vstat, init)
		return typecheck.Expr(vstat)
	}
	var_ := typecheck.Temp(n.Type())
	anylit(n, var_, init)
	return var_
}

// initContext is the context in which static data is populated.
// It is either in an init function or in any other function.
// Static data populated in an init function will be written either
// zero times (as a readonly, static data symbol) or
// one time (during init function execution).
// Either way, there is no opportunity for races or further modification,
// so the data can be written to a (possibly readonly) data symbol.
// Static data populated in any other function needs to be local to
// that function to allow multiple instances of that function
// to execute concurrently without clobbering each others' data.
type initContext uint8

const (
	inInitFunction initContext = iota
	inNonInitFunction
)

func (c initContext) String() string {
	if c == inInitFunction {
		return "inInitFunction"
	}
	return "inNonInitFunction"
}

// readonlystaticname returns a name backed by a (writable) static data symbol.
func readonlystaticname(t *types.Type) *ir.Name {
	n := staticinit.StaticName(t)
	n.MarkReadonly()
	n.Linksym().Set(obj.AttrContentAddressable, true)
	return n
}

func isSimpleName(nn ir.Node) bool {
	if nn.Op() != ir.ONAME {
		return false
	}
	n := nn.(*ir.Name)
	return n.Class != ir.PAUTOHEAP && n.Class != ir.PEXTERN
}

func litas(l ir.Node, r ir.Node, init *ir.Nodes) {
	appendWalkStmt(init, ir.NewAssignStmt(base.Pos, l, r))
}

// initGenType is a bitmap indicating the types of generation that will occur for a static value.
type initGenType uint8

const (
	initDynamic initGenType = 1 << iota // contains some dynamic values, for which init code will be generated
	initConst                           // contains some constant values, which may be written into data symbols
)

// getdyn calculates the initGenType for n.
// If top is false, getdyn is recursing.
func getdyn(n ir.Node, top bool) initGenType {
	switch n.Op() {
	default:
		if ir.IsConstNode(n) {
			return initConst
		}
		return initDynamic

	case ir.OSLICELIT:
		n := n.(*ir.CompLitExpr)
		if !top {
			return initDynamic
		}
		if n.Len/4 > int64(len(n.List)) {
			// <25% of entries have explicit values.
			// Very rough estimation, it takes 4 bytes of instructions
			// to initialize 1 byte of result. So don't use a static
			// initializer if the dynamic initialization code would be
			// smaller than the static value.
			// See issue 23780.
			return initDynamic
		}

	case ir.OARRAYLIT, ir.OSTRUCTLIT:
	}
	lit := n.(*ir.CompLitExpr)

	var mode initGenType
	for _, n1 := range lit.List {
		switch n1.Op() {
		case ir.OKEY:
			n1 = n1.(*ir.KeyExpr).Value
		case ir.OSTRUCTKEY:
			n1 = n1.(*ir.StructKeyExpr).Value
		}
		mode |= getdyn(n1, false)
		if mode == initDynamic|initConst {
			break
		}
	}
	return mode
}

// isStaticCompositeLiteral reports whether n is a compile-time constant.
func isStaticCompositeLiteral(n ir.Node) bool {
	switch n.Op() {
	case ir.OSLICELIT:
		return false
	case ir.OARRAYLIT:
		n := n.(*ir.CompLitExpr)
		for _, r := range n.List {
			if r.Op() == ir.OKEY {
				r = r.(*ir.KeyExpr).Value
			}
			if !isStaticCompositeLiteral(r) {
				return false
			}
		}
		return true
	case ir.OSTRUCTLIT:
		n := n.(*ir.CompLitExpr)
		for _, r := range n.List {
			r := r.(*ir.StructKeyExpr)
			if !isStaticCompositeLiteral(r.Value) {
				return false
			}
		}
		return true
	case ir.OLITERAL, ir.ONIL:
		return true
	case ir.OCONVIFACE:
		// See staticassign's OCONVIFACE case for comments.
		n := n.(*ir.ConvExpr)
		val := ir.Node(n)
		for val.Op() == ir.OCONVIFACE {
			val = val.(*ir.ConvExpr).X
		}
		if val.Type().IsInterface() {
			return val.Op() == ir.ONIL
		}
		if types.IsDirectIface(val.Type()) && val.Op() == ir.ONIL {
			return true
		}
		return isStaticCompositeLiteral(val)
	}
	return false
}

// initKind is a kind of static initialization: static, dynamic, or local.
// Static initialization represents literals and
// literal components of composite literals.
// Dynamic initialization represents non-literals and
// non-literal components of composite literals.
// LocalCode initialization represents initialization
// that occurs purely in generated code local to the function of use.
// Initialization code is sometimes generated in passes,
// first static then dynamic.
type initKind uint8

const (
	initKindStatic initKind = iota + 1
	initKindDynamic
	initKindLocalCode
)

// fixedlit handles struct, array, and slice literals.
// TODO: expand documentation.
func fixedlit(ctxt initContext, kind initKind, n *ir.CompLitExpr, var_ ir.Node, init *ir.Nodes) {
	isBlank := var_ == ir.BlankNode
	var splitnode func(ir.Node) (a ir.Node, value ir.Node)
	switch n.Op() {
	case ir.OARRAYLIT, ir.OSLICELIT:
		var k int64
		splitnode = func(r ir.Node) (ir.Node, ir.Node) {
			if r.Op() == ir.OKEY {
				kv := r.(*ir.KeyExpr)
				k = typecheck.IndexConst(kv.Key)
				if k < 0 {
					base.Fatalf("fixedlit: invalid index %v", kv.Key)
				}
				r = kv.Value
			}
			a := ir.NewIndexExpr(base.Pos, var_, ir.NewInt(k))
			k++
			if isBlank {
				return ir.BlankNode, r
			}
			return a, r
		}
	case ir.OSTRUCTLIT:
		splitnode = func(rn ir.Node) (ir.Node, ir.Node) {
			r := rn.(*ir.StructKeyExpr)
			if r.Field.IsBlank() || isBlank {
				return ir.BlankNode, r.Value
			}
			ir.SetPos(r)
			return ir.NewSelectorExpr(base.Pos, ir.ODOT, var_, r.Field), r.Value
		}
	default:
		base.Fatalf("fixedlit bad op: %v", n.Op())
	}

	for _, r := range n.List {
		a, value := splitnode(r)
		if a == ir.BlankNode && !staticinit.AnySideEffects(value) {
			// Discard.
			continue
		}

		switch value.Op() {
		case ir.OSLICELIT:
			value := value.(*ir.CompLitExpr)
			if (kind == initKindStatic && ctxt == inNonInitFunction) || (kind == initKindDynamic && ctxt == inInitFunction) {
				slicelit(ctxt, value, a, init)
				continue
			}

		case ir.OARRAYLIT, ir.OSTRUCTLIT:
			value := value.(*ir.CompLitExpr)
			fixedlit(ctxt, kind, value, a, init)
			continue
		}

		islit := ir.IsConstNode(value)
		if (kind == initKindStatic && !islit) || (kind == initKindDynamic && islit) {
			continue
		}

		// build list of assignments: var[index] = expr
		ir.SetPos(a)
		as := ir.NewAssignStmt(base.Pos, a, value)
		as = typecheck.Stmt(as).(*ir.AssignStmt)
		switch kind {
		case initKindStatic:
			genAsStatic(as)
		case initKindDynamic, initKindLocalCode:
			a = orderStmtInPlace(as, map[string][]*ir.Name{})
			a = walkStmt(a)
			init.Append(a)
		default:
			base.Fatalf("fixedlit: bad kind %d", kind)
		}

	}
}

func isSmallSliceLit(n *ir.CompLitExpr) bool {
	if n.Op() != ir.OSLICELIT {
		return false
	}

	return n.Type().Elem().Width == 0 || n.Len <= ir.MaxSmallArraySize/n.Type().Elem().Width
}

func slicelit(ctxt initContext, n *ir.CompLitExpr, var_ ir.Node, init *ir.Nodes) {
	// make an array type corresponding the number of elements we have
	t := types.NewArray(n.Type().Elem(), n.Len)
	types.CalcSize(t)

	if ctxt == inNonInitFunction {
		// put everything into static array
		vstat := staticinit.StaticName(t)

		fixedlit(ctxt, initKindStatic, n, vstat, init)
		fixedlit(ctxt, initKindDynamic, n, vstat, init)

		// copy static to slice
		var_ = typecheck.AssignExpr(var_)
		name, offset, ok := staticinit.StaticLoc(var_)
		if !ok || name.Class != ir.PEXTERN {
			base.Fatalf("slicelit: %v", var_)
		}
		staticdata.InitSlice(name, offset, vstat, t.NumElem())
		return
	}

	// recipe for var = []t{...}
	// 1. make a static array
	//	var vstat [...]t
	// 2. assign (data statements) the constant part
	//	vstat = constpart{}
	// 3. make an auto pointer to array and allocate heap to it
	//	var vauto *[...]t = new([...]t)
	// 4. copy the static array to the auto array
	//	*vauto = vstat
	// 5. for each dynamic part assign to the array
	//	vauto[i] = dynamic part
	// 6. assign slice of allocated heap to var
	//	var = vauto[:]
	//
	// an optimization is done if there is no constant part
	//	3. var vauto *[...]t = new([...]t)
	//	5. vauto[i] = dynamic part
	//	6. var = vauto[:]

	// if the literal contains constants,
	// make static initialized array (1),(2)
	var vstat ir.Node

	mode := getdyn(n, true)
	if mode&initConst != 0 && !isSmallSliceLit(n) {
		if ctxt == inInitFunction {
			vstat = readonlystaticname(t)
		} else {
			vstat = staticinit.StaticName(t)
		}
		fixedlit(ctxt, initKindStatic, n, vstat, init)
	}

	// make new auto *array (3 declare)
	vauto := typecheck.Temp(types.NewPtr(t))

	// set auto to point at new temp or heap (3 assign)
	var a ir.Node
	if x := n.Prealloc; x != nil {
		// temp allocated during order.go for dddarg
		if !types.Identical(t, x.Type()) {
			panic("dotdotdot base type does not match order's assigned type")
		}

		if vstat == nil {
			a = ir.NewAssignStmt(base.Pos, x, nil)
			a = typecheck.Stmt(a)
			init.Append(a) // zero new temp
		} else {
			// Declare that we're about to initialize all of x.
			// (Which happens at the *vauto = vstat below.)
			init.Append(ir.NewUnaryExpr(base.Pos, ir.OVARDEF, x))
		}

		a = typecheck.NodAddr(x)
	} else if n.Esc() == ir.EscNone {
		a = typecheck.Temp(t)
		if vstat == nil {
			a = ir.NewAssignStmt(base.Pos, typecheck.Temp(t), nil)
			a = typecheck.Stmt(a)
			init.Append(a) // zero new temp
			a = a.(*ir.AssignStmt).X
		} else {
			init.Append(ir.NewUnaryExpr(base.Pos, ir.OVARDEF, a))
		}

		a = typecheck.NodAddr(a)
	} else {
		a = ir.NewUnaryExpr(base.Pos, ir.ONEW, ir.TypeNode(t))
	}
	appendWalkStmt(init, ir.NewAssignStmt(base.Pos, vauto, a))

	if vstat != nil {
		// copy static to heap (4)
		a = ir.NewStarExpr(base.Pos, vauto)
		appendWalkStmt(init, ir.NewAssignStmt(base.Pos, a, vstat))
	}

	// put dynamics into array (5)
	var index int64
	for _, value := range n.List {
		if value.Op() == ir.OKEY {
			kv := value.(*ir.KeyExpr)
			index = typecheck.IndexConst(kv.Key)
			if index < 0 {
				base.Fatalf("slicelit: invalid index %v", kv.Key)
			}
			value = kv.Value
		}
		a := ir.NewIndexExpr(base.Pos, vauto, ir.NewInt(index))
		a.SetBounded(true)
		index++

		// TODO need to check bounds?

		switch value.Op() {
		case ir.OSLICELIT:
			break

		case ir.OARRAYLIT, ir.OSTRUCTLIT:
			value := value.(*ir.CompLitExpr)
			k := initKindDynamic
			if vstat == nil {
				// Generate both static and dynamic initializations.
				// See issue #31987.
				k = initKindLocalCode
			}
			fixedlit(ctxt, k, value, a, init)
			continue
		}

		if vstat != nil && ir.IsConstNode(value) { // already set by copy from static value
			continue
		}

		// build list of vauto[c] = expr
		ir.SetPos(value)
		as := typecheck.Stmt(ir.NewAssignStmt(base.Pos, a, value))
		as = orderStmtInPlace(as, map[string][]*ir.Name{})
		as = walkStmt(as)
		init.Append(as)
	}

	// make slice out of heap (6)
	a = ir.NewAssignStmt(base.Pos, var_, ir.NewSliceExpr(base.Pos, ir.OSLICE, vauto, nil, nil, nil))

	a = typecheck.Stmt(a)
	a = orderStmtInPlace(a, map[string][]*ir.Name{})
	a = walkStmt(a)
	init.Append(a)
}

func maplit(n *ir.CompLitExpr, m ir.Node, init *ir.Nodes) {
	// make the map var
	a := ir.NewCallExpr(base.Pos, ir.OMAKE, nil, nil)
	a.SetEsc(n.Esc())
	a.Args = []ir.Node{ir.TypeNode(n.Type()), ir.NewInt(int64(len(n.List)))}
	litas(m, a, init)

	entries := n.List

	// The order pass already removed any dynamic (runtime-computed) entries.
	// All remaining entries are static. Double-check that.
	for _, r := range entries {
		r := r.(*ir.KeyExpr)
		if !isStaticCompositeLiteral(r.Key) || !isStaticCompositeLiteral(r.Value) {
			base.Fatalf("maplit: entry is not a literal: %v", r)
		}
	}

	if len(entries) > 25 {
		// For a large number of entries, put them in an array and loop.

		// build types [count]Tindex and [count]Tvalue
		tk := types.NewArray(n.Type().Key(), int64(len(entries)))
		te := types.NewArray(n.Type().Elem(), int64(len(entries)))

		tk.SetNoalg(true)
		te.SetNoalg(true)

		types.CalcSize(tk)
		types.CalcSize(te)

		// make and initialize static arrays
		vstatk := readonlystaticname(tk)
		vstate := readonlystaticname(te)

		datak := ir.NewCompLitExpr(base.Pos, ir.OARRAYLIT, nil, nil)
		datae := ir.NewCompLitExpr(base.Pos, ir.OARRAYLIT, nil, nil)
		for _, r := range entries {
			r := r.(*ir.KeyExpr)
			datak.List.Append(r.Key)
			datae.List.Append(r.Value)
		}
		fixedlit(inInitFunction, initKindStatic, datak, vstatk, init)
		fixedlit(inInitFunction, initKindStatic, datae, vstate, init)

		// loop adding structure elements to map
		// for i = 0; i < len(vstatk); i++ {
		//	map[vstatk[i]] = vstate[i]
		// }
		i := typecheck.Temp(types.Types[types.TINT])
		rhs := ir.NewIndexExpr(base.Pos, vstate, i)
		rhs.SetBounded(true)

		kidx := ir.NewIndexExpr(base.Pos, vstatk, i)
		kidx.SetBounded(true)
		lhs := ir.NewIndexExpr(base.Pos, m, kidx)

		zero := ir.NewAssignStmt(base.Pos, i, ir.NewInt(0))
		cond := ir.NewBinaryExpr(base.Pos, ir.OLT, i, ir.NewInt(tk.NumElem()))
		incr := ir.NewAssignStmt(base.Pos, i, ir.NewBinaryExpr(base.Pos, ir.OADD, i, ir.NewInt(1)))
		body := ir.NewAssignStmt(base.Pos, lhs, rhs)

		loop := ir.NewForStmt(base.Pos, nil, cond, incr, nil)
		loop.Body = []ir.Node{body}
		*loop.PtrInit() = []ir.Node{zero}

		appendWalkStmt(init, loop)
		return
	}
	// For a small number of entries, just add them directly.

	// Build list of var[c] = expr.
	// Use temporaries so that mapassign1 can have addressable key, elem.
	// TODO(josharian): avoid map key temporaries for mapfast_* assignments with literal keys.
	tmpkey := typecheck.Temp(m.Type().Key())
	tmpelem := typecheck.Temp(m.Type().Elem())

	for _, r := range entries {
		r := r.(*ir.KeyExpr)
		index, elem := r.Key, r.Value

		ir.SetPos(index)
		appendWalkStmt(init, ir.NewAssignStmt(base.Pos, tmpkey, index))

		ir.SetPos(elem)
		appendWalkStmt(init, ir.NewAssignStmt(base.Pos, tmpelem, elem))

		ir.SetPos(tmpelem)
		appendWalkStmt(init, ir.NewAssignStmt(base.Pos, ir.NewIndexExpr(base.Pos, m, tmpkey), tmpelem))
	}

	appendWalkStmt(init, ir.NewUnaryExpr(base.Pos, ir.OVARKILL, tmpkey))
	appendWalkStmt(init, ir.NewUnaryExpr(base.Pos, ir.OVARKILL, tmpelem))
}

func anylit(n ir.Node, var_ ir.Node, init *ir.Nodes) {
	t := n.Type()
	switch n.Op() {
	default:
		base.Fatalf("anylit: not lit, op=%v node=%v", n.Op(), n)

	case ir.ONAME:
		n := n.(*ir.Name)
		appendWalkStmt(init, ir.NewAssignStmt(base.Pos, var_, n))

	case ir.OMETHEXPR:
		n := n.(*ir.SelectorExpr)
		anylit(n.FuncName(), var_, init)

	case ir.OPTRLIT:
		n := n.(*ir.AddrExpr)
		if !t.IsPtr() {
			base.Fatalf("anylit: not ptr")
		}

		var r ir.Node
		if n.Prealloc != nil {
			// n.Right is stack temporary used as backing store.
			appendWalkStmt(init, ir.NewAssignStmt(base.Pos, n.Prealloc, nil)) // zero backing store, just in case (#18410)
			r = typecheck.NodAddr(n.Prealloc)
		} else {
			r = ir.NewUnaryExpr(base.Pos, ir.ONEW, ir.TypeNode(n.X.Type()))
			r.SetEsc(n.Esc())
		}
		appendWalkStmt(init, ir.NewAssignStmt(base.Pos, var_, r))

		var_ = ir.NewStarExpr(base.Pos, var_)
		var_ = typecheck.AssignExpr(var_)
		anylit(n.X, var_, init)

	case ir.OSTRUCTLIT, ir.OARRAYLIT:
		n := n.(*ir.CompLitExpr)
		if !t.IsStruct() && !t.IsArray() {
			base.Fatalf("anylit: not struct/array")
		}

		if isSimpleName(var_) && len(n.List) > 4 {
			// lay out static data
			vstat := readonlystaticname(t)

			ctxt := inInitFunction
			if n.Op() == ir.OARRAYLIT {
				ctxt = inNonInitFunction
			}
			fixedlit(ctxt, initKindStatic, n, vstat, init)

			// copy static to var
			appendWalkStmt(init, ir.NewAssignStmt(base.Pos, var_, vstat))

			// add expressions to automatic
			fixedlit(inInitFunction, initKindDynamic, n, var_, init)
			break
		}

		var components int64
		if n.Op() == ir.OARRAYLIT {
			components = t.NumElem()
		} else {
			components = int64(t.NumFields())
		}
		// initialization of an array or struct with unspecified components (missing fields or arrays)
		if isSimpleName(var_) || int64(len(n.List)) < components {
			appendWalkStmt(init, ir.NewAssignStmt(base.Pos, var_, nil))
		}

		fixedlit(inInitFunction, initKindLocalCode, n, var_, init)

	case ir.OSLICELIT:
		n := n.(*ir.CompLitExpr)
		slicelit(inInitFunction, n, var_, init)

	case ir.OMAPLIT:
		n := n.(*ir.CompLitExpr)
		if !t.IsMap() {
			base.Fatalf("anylit: not map")
		}
		maplit(n, var_, init)
	}
}

// oaslit handles special composite literal assignments.
// It returns true if n's effects have been added to init,
// in which case n should be dropped from the program by the caller.
func oaslit(n *ir.AssignStmt, init *ir.Nodes) bool {
	if n.X == nil || n.Y == nil {
		// not a special composite literal assignment
		return false
	}
	if n.X.Type() == nil || n.Y.Type() == nil {
		// not a special composite literal assignment
		return false
	}
	if !isSimpleName(n.X) {
		// not a special composite literal assignment
		return false
	}
	x := n.X.(*ir.Name)
	if !types.Identical(n.X.Type(), n.Y.Type()) {
		// not a special composite literal assignment
		return false
	}

	switch n.Y.Op() {
	default:
		// not a special composite literal assignment
		return false

	case ir.OSTRUCTLIT, ir.OARRAYLIT, ir.OSLICELIT, ir.OMAPLIT:
		if ir.Any(n.Y, func(y ir.Node) bool { return ir.Uses(y, x) }) {
			// not a special composite literal assignment
			return false
		}
		anylit(n.Y, n.X, init)
	}

	return true
}

func genAsStatic(as *ir.AssignStmt) {
	if as.X.Type() == nil {
		base.Fatalf("genAsStatic as.Left not typechecked")
	}

	name, offset, ok := staticinit.StaticLoc(as.X)
	if !ok || (name.Class != ir.PEXTERN && as.X != ir.BlankNode) {
		base.Fatalf("genAsStatic: lhs %v", as.X)
	}

	switch r := as.Y; r.Op() {
	case ir.OLITERAL:
		staticdata.InitConst(name, offset, r, int(r.Type().Width))
		return
	case ir.OMETHEXPR:
		r := r.(*ir.SelectorExpr)
		staticdata.InitFunc(name, offset, r.FuncName())
		return
	case ir.ONAME:
		r := r.(*ir.Name)
		if r.Offset_ != 0 {
			base.Fatalf("genAsStatic %+v", as)
		}
		if r.Class == ir.PFUNC {
			staticdata.InitFunc(name, offset, r)
			return
		}
	}
	base.Fatalf("genAsStatic: rhs %v", as.Y)
}
