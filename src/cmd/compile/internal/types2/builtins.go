// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements typechecking of builtin function calls.

package types2

import (
	"cmd/compile/internal/syntax"
	"go/constant"
	"go/token"
	. "internal/types/errors"
)

// builtin type-checks a call to the built-in specified by id and
// reports whether the call is valid, with *x holding the result;
// but x.expr is not set. If the call is invalid, the result is
// false, and *x is undefined.
func (check *Checker) builtin(x *operand, call *syntax.CallExpr, id builtinId) (_ bool) {
	// append is the only built-in that permits the use of ... for the last argument
	bin := predeclaredFuncs[id]
	if call.HasDots && id != _Append {
		//check.errorf(call.Ellipsis, invalidOp + "invalid use of ... with built-in %s", bin.name)
		check.errorf(call, InvalidDotDotDot, invalidOp+"invalid use of ... with built-in %s", bin.name)
		check.use(call.ArgList...)
		return
	}

	// For len(x) and cap(x) we need to know if x contains any function calls or
	// receive operations. Save/restore current setting and set hasCallOrRecv to
	// false for the evaluation of x so that we can check it afterwards.
	// Note: We must do this _before_ calling exprList because exprList evaluates
	//       all arguments.
	if id == _Len || id == _Cap {
		defer func(b bool) {
			check.hasCallOrRecv = b
		}(check.hasCallOrRecv)
		check.hasCallOrRecv = false
	}

	// determine actual arguments
	var arg func(*operand, int) // TODO(gri) remove use of arg getter in favor of using xlist directly
	nargs := len(call.ArgList)
	switch id {
	default:
		// make argument getter
		xlist := check.exprList(call.ArgList)
		arg = func(x *operand, i int) { *x = *xlist[i] }
		nargs = len(xlist)
		// evaluate first argument, if present
		if nargs > 0 {
			arg(x, 0)
			if x.mode == invalid {
				return
			}
		}
	case _Make, _New, _Offsetof, _Trace:
		// arguments require special handling
	}

	// check argument count
	{
		msg := ""
		if nargs < bin.nargs {
			msg = "not enough"
		} else if !bin.variadic && nargs > bin.nargs {
			msg = "too many"
		}
		if msg != "" {
			check.errorf(call, WrongArgCount, invalidOp+"%s arguments for %v (expected %d, found %d)", msg, call, bin.nargs, nargs)
			return
		}
	}

	switch id {
	case _Append:
		// append(s S, x ...T) S, where T is the element type of S
		// spec: "The variadic function append appends zero or more values x to s of type
		// S, which must be a slice type, and returns the resulting slice, also of type S.
		// The values x are passed to a parameter of type ...T where T is the element type
		// of S and the respective parameter passing rules apply."
		S := x.typ
		var T Type
		if s, _ := coreType(S).(*Slice); s != nil {
			T = s.elem
		} else {
			var cause string
			switch {
			case x.isNil():
				cause = "have untyped nil"
			case isTypeParam(S):
				if u := coreType(S); u != nil {
					cause = check.sprintf("%s has core type %s", x, u)
				} else {
					cause = check.sprintf("%s has no core type", x)
				}
			default:
				cause = check.sprintf("have %s", x)
			}
			// don't use invalidArg prefix here as it would repeat "argument" in the error message
			check.errorf(x, InvalidAppend, "first argument to append must be a slice; %s", cause)
			return
		}

		// remember arguments that have been evaluated already
		alist := []operand{*x}

		// spec: "As a special case, append also accepts a first argument assignable
		// to type []byte with a second argument of string type followed by ... .
		// This form appends the bytes of the string.
		if nargs == 2 && call.HasDots {
			if ok, _ := x.assignableTo(check, NewSlice(universeByte), nil); ok {
				arg(x, 1)
				if x.mode == invalid {
					return
				}
				if t := coreString(x.typ); t != nil && isString(t) {
					if check.recordTypes() {
						sig := makeSig(S, S, x.typ)
						sig.variadic = true
						check.recordBuiltinType(call.Fun, sig)
					}
					x.mode = value
					x.typ = S
					break
				}
				alist = append(alist, *x)
				// fallthrough
			}
		}

		// check general case by creating custom signature
		sig := makeSig(S, S, NewSlice(T)) // []T required for variadic signature
		sig.variadic = true
		var alist2 []*operand
		// convert []operand to []*operand
		for i := range alist {
			alist2 = append(alist2, &alist[i])
		}
		for i := len(alist); i < nargs; i++ {
			var x operand
			arg(&x, i)
			alist2 = append(alist2, &x)
		}
		check.arguments(call, sig, nil, nil, alist2) // discard result (we know the result type)
		// ok to continue even if check.arguments reported errors

		x.mode = value
		x.typ = S
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, sig)
		}

	case _Cap, _Len:
		// cap(x)
		// len(x)
		mode := invalid
		var val constant.Value
		switch t := arrayPtrDeref(under(x.typ)).(type) {
		case *Basic:
			if isString(t) && id == _Len {
				if x.mode == constant_ {
					mode = constant_
					val = constant.MakeInt64(int64(len(constant.StringVal(x.val))))
				} else {
					mode = value
				}
			}

		case *Array:
			mode = value
			// spec: "The expressions len(s) and cap(s) are constants
			// if the type of s is an array or pointer to an array and
			// the expression s does not contain channel receives or
			// function calls; in this case s is not evaluated."
			if !check.hasCallOrRecv {
				mode = constant_
				if t.len >= 0 {
					val = constant.MakeInt64(t.len)
				} else {
					val = constant.MakeUnknown()
				}
			}

		case *Slice, *Chan:
			mode = value

		case *Map:
			if id == _Len {
				mode = value
			}

		case *Interface:
			if !isTypeParam(x.typ) {
				break
			}
			if t.typeSet().underIs(func(t Type) bool {
				switch t := arrayPtrDeref(t).(type) {
				case *Basic:
					if isString(t) && id == _Len {
						return true
					}
				case *Array, *Slice, *Chan:
					return true
				case *Map:
					if id == _Len {
						return true
					}
				}
				return false
			}) {
				mode = value
			}
		}

		if mode == invalid && under(x.typ) != Typ[Invalid] {
			code := InvalidCap
			if id == _Len {
				code = InvalidLen
			}
			check.errorf(x, code, invalidArg+"%s for %s", x, bin.name)
			return
		}

		// record the signature before changing x.typ
		if check.recordTypes() && mode != constant_ {
			check.recordBuiltinType(call.Fun, makeSig(Typ[Int], x.typ))
		}

		x.mode = mode
		x.typ = Typ[Int]
		x.val = val

	case _Clear:
		// clear(m)
		if !check.verifyVersionf(check.pkg, call.Fun, go1_21, "clear") {
			return
		}

		if !underIs(x.typ, func(u Type) bool {
			switch u.(type) {
			case *Map, *Slice:
				return true
			}
			check.errorf(x, InvalidClear, invalidArg+"cannot clear %s: argument must be (or constrained by) map or slice", x)
			return false
		}) {
			return
		}

		x.mode = novalue
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(nil, x.typ))
		}

	case _Close:
		// close(c)
		if !underIs(x.typ, func(u Type) bool {
			uch, _ := u.(*Chan)
			if uch == nil {
				check.errorf(x, InvalidClose, invalidOp+"cannot close non-channel %s", x)
				return false
			}
			if uch.dir == RecvOnly {
				check.errorf(x, InvalidClose, invalidOp+"cannot close receive-only channel %s", x)
				return false
			}
			return true
		}) {
			return
		}
		x.mode = novalue
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(nil, x.typ))
		}

	case _Complex:
		// complex(x, y floatT) complexT
		var y operand
		arg(&y, 1)
		if y.mode == invalid {
			return
		}

		// convert or check untyped arguments
		d := 0
		if isUntyped(x.typ) {
			d |= 1
		}
		if isUntyped(y.typ) {
			d |= 2
		}
		switch d {
		case 0:
			// x and y are typed => nothing to do
		case 1:
			// only x is untyped => convert to type of y
			check.convertUntyped(x, y.typ)
		case 2:
			// only y is untyped => convert to type of x
			check.convertUntyped(&y, x.typ)
		case 3:
			// x and y are untyped =>
			// 1) if both are constants, convert them to untyped
			//    floating-point numbers if possible,
			// 2) if one of them is not constant (possible because
			//    it contains a shift that is yet untyped), convert
			//    both of them to float64 since they must have the
			//    same type to succeed (this will result in an error
			//    because shifts of floats are not permitted)
			if x.mode == constant_ && y.mode == constant_ {
				toFloat := func(x *operand) {
					if isNumeric(x.typ) && constant.Sign(constant.Imag(x.val)) == 0 {
						x.typ = Typ[UntypedFloat]
					}
				}
				toFloat(x)
				toFloat(&y)
			} else {
				check.convertUntyped(x, Typ[Float64])
				check.convertUntyped(&y, Typ[Float64])
				// x and y should be invalid now, but be conservative
				// and check below
			}
		}
		if x.mode == invalid || y.mode == invalid {
			return
		}

		// both argument types must be identical
		if !Identical(x.typ, y.typ) {
			check.errorf(x, InvalidComplex, invalidOp+"%v (mismatched types %s and %s)", call, x.typ, y.typ)
			return
		}

		// the argument types must be of floating-point type
		// (applyTypeFunc never calls f with a type parameter)
		f := func(typ Type) Type {
			assert(!isTypeParam(typ))
			if t, _ := under(typ).(*Basic); t != nil {
				switch t.kind {
				case Float32:
					return Typ[Complex64]
				case Float64:
					return Typ[Complex128]
				case UntypedFloat:
					return Typ[UntypedComplex]
				}
			}
			return nil
		}
		resTyp := check.applyTypeFunc(f, x, id)
		if resTyp == nil {
			check.errorf(x, InvalidComplex, invalidArg+"arguments have type %s, expected floating-point", x.typ)
			return
		}

		// if both arguments are constants, the result is a constant
		if x.mode == constant_ && y.mode == constant_ {
			x.val = constant.BinaryOp(constant.ToFloat(x.val), token.ADD, constant.MakeImag(constant.ToFloat(y.val)))
		} else {
			x.mode = value
		}

		if check.recordTypes() && x.mode != constant_ {
			check.recordBuiltinType(call.Fun, makeSig(resTyp, x.typ, x.typ))
		}

		x.typ = resTyp

	case _Copy:
		// copy(x, y []T) int
		dst, _ := coreType(x.typ).(*Slice)

		var y operand
		arg(&y, 1)
		if y.mode == invalid {
			return
		}
		src0 := coreString(y.typ)
		if src0 != nil && isString(src0) {
			src0 = NewSlice(universeByte)
		}
		src, _ := src0.(*Slice)

		if dst == nil || src == nil {
			check.errorf(x, InvalidCopy, invalidArg+"copy expects slice arguments; found %s and %s", x, &y)
			return
		}

		if !Identical(dst.elem, src.elem) {
			check.errorf(x, InvalidCopy, invalidArg+"arguments to copy %s and %s have different element types %s and %s", x, &y, dst.elem, src.elem)
			return
		}

		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(Typ[Int], x.typ, y.typ))
		}
		x.mode = value
		x.typ = Typ[Int]

	case _Delete:
		// delete(map_, key)
		// map_ must be a map type or a type parameter describing map types.
		// The key cannot be a type parameter for now.
		map_ := x.typ
		var key Type
		if !underIs(map_, func(u Type) bool {
			map_, _ := u.(*Map)
			if map_ == nil {
				check.errorf(x, InvalidDelete, invalidArg+"%s is not a map", x)
				return false
			}
			if key != nil && !Identical(map_.key, key) {
				check.errorf(x, InvalidDelete, invalidArg+"maps of %s must have identical key types", x)
				return false
			}
			key = map_.key
			return true
		}) {
			return
		}

		arg(x, 1) // k
		if x.mode == invalid {
			return
		}

		check.assignment(x, key, "argument to delete")
		if x.mode == invalid {
			return
		}

		x.mode = novalue
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(nil, map_, key))
		}

	case _Imag, _Real:
		// imag(complexT) floatT
		// real(complexT) floatT

		// convert or check untyped argument
		if isUntyped(x.typ) {
			if x.mode == constant_ {
				// an untyped constant number can always be considered
				// as a complex constant
				if isNumeric(x.typ) {
					x.typ = Typ[UntypedComplex]
				}
			} else {
				// an untyped non-constant argument may appear if
				// it contains a (yet untyped non-constant) shift
				// expression: convert it to complex128 which will
				// result in an error (shift of complex value)
				check.convertUntyped(x, Typ[Complex128])
				// x should be invalid now, but be conservative and check
				if x.mode == invalid {
					return
				}
			}
		}

		// the argument must be of complex type
		// (applyTypeFunc never calls f with a type parameter)
		f := func(typ Type) Type {
			assert(!isTypeParam(typ))
			if t, _ := under(typ).(*Basic); t != nil {
				switch t.kind {
				case Complex64:
					return Typ[Float32]
				case Complex128:
					return Typ[Float64]
				case UntypedComplex:
					return Typ[UntypedFloat]
				}
			}
			return nil
		}
		resTyp := check.applyTypeFunc(f, x, id)
		if resTyp == nil {
			code := InvalidImag
			if id == _Real {
				code = InvalidReal
			}
			check.errorf(x, code, invalidArg+"argument has type %s, expected complex type", x.typ)
			return
		}

		// if the argument is a constant, the result is a constant
		if x.mode == constant_ {
			if id == _Real {
				x.val = constant.Real(x.val)
			} else {
				x.val = constant.Imag(x.val)
			}
		} else {
			x.mode = value
		}

		if check.recordTypes() && x.mode != constant_ {
			check.recordBuiltinType(call.Fun, makeSig(resTyp, x.typ))
		}

		x.typ = resTyp

	case _Make:
		// make(T, n)
		// make(T, n, m)
		// (no argument evaluated yet)
		arg0 := call.ArgList[0]
		T := check.varType(arg0)
		if T == Typ[Invalid] {
			return
		}

		var min int // minimum number of arguments
		switch coreType(T).(type) {
		case *Slice:
			min = 2
		case *Map, *Chan:
			min = 1
		case nil:
			check.errorf(arg0, InvalidMake, invalidArg+"cannot make %s: no core type", arg0)
			return
		default:
			check.errorf(arg0, InvalidMake, invalidArg+"cannot make %s; type must be slice, map, or channel", arg0)
			return
		}
		if nargs < min || min+1 < nargs {
			check.errorf(call, WrongArgCount, invalidOp+"%v expects %d or %d arguments; found %d", call, min, min+1, nargs)
			return
		}

		types := []Type{T}
		var sizes []int64 // constant integer arguments, if any
		for _, arg := range call.ArgList[1:] {
			typ, size := check.index(arg, -1) // ok to continue with typ == Typ[Invalid]
			types = append(types, typ)
			if size >= 0 {
				sizes = append(sizes, size)
			}
		}
		if len(sizes) == 2 && sizes[0] > sizes[1] {
			check.error(call.ArgList[1], SwappedMakeArgs, invalidArg+"length and capacity swapped")
			// safe to continue
		}
		x.mode = value
		x.typ = T
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(x.typ, types...))
		}

	case _New:
		// new(T)
		// (no argument evaluated yet)
		T := check.varType(call.ArgList[0])
		if T == Typ[Invalid] {
			return
		}

		x.mode = value
		x.typ = &Pointer{base: T}
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(x.typ, T))
		}

	case _Panic:
		// panic(x)
		// record panic call if inside a function with result parameters
		// (for use in Checker.isTerminating)
		if check.sig != nil && check.sig.results.Len() > 0 {
			// function has result parameters
			p := check.isPanic
			if p == nil {
				// allocate lazily
				p = make(map[*syntax.CallExpr]bool)
				check.isPanic = p
			}
			p[call] = true
		}

		check.assignment(x, &emptyInterface, "argument to panic")
		if x.mode == invalid {
			return
		}

		x.mode = novalue
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(nil, &emptyInterface))
		}

	case _Print, _Println:
		// print(x, y, ...)
		// println(x, y, ...)
		var params []Type
		if nargs > 0 {
			params = make([]Type, nargs)
			for i := 0; i < nargs; i++ {
				if i > 0 {
					arg(x, i) // first argument already evaluated
				}
				check.assignment(x, nil, "argument to "+predeclaredFuncs[id].name)
				if x.mode == invalid {
					// TODO(gri) "use" all arguments?
					return
				}
				params[i] = x.typ
			}
		}

		x.mode = novalue
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(nil, params...))
		}

	case _Recover:
		// recover() interface{}
		x.mode = value
		x.typ = &emptyInterface
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(x.typ))
		}

	case _Add:
		// unsafe.Add(ptr unsafe.Pointer, len IntegerType) unsafe.Pointer
		if !check.verifyVersionf(check.pkg, call.Fun, go1_17, "unsafe.Add") {
			return
		}

		check.assignment(x, Typ[UnsafePointer], "argument to unsafe.Add")
		if x.mode == invalid {
			return
		}

		var y operand
		arg(&y, 1)
		if !check.isValidIndex(&y, InvalidUnsafeAdd, "length", true) {
			return
		}

		x.mode = value
		x.typ = Typ[UnsafePointer]
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(x.typ, x.typ, y.typ))
		}

	case _Alignof:
		// unsafe.Alignof(x T) uintptr
		check.assignment(x, nil, "argument to unsafe.Alignof")
		if x.mode == invalid {
			return
		}

		if hasVarSize(x.typ, nil) {
			x.mode = value
			if check.recordTypes() {
				check.recordBuiltinType(call.Fun, makeSig(Typ[Uintptr], x.typ))
			}
		} else {
			x.mode = constant_
			x.val = constant.MakeInt64(check.conf.alignof(x.typ))
			// result is constant - no need to record signature
		}
		x.typ = Typ[Uintptr]

	case _Offsetof:
		// unsafe.Offsetof(x T) uintptr, where x must be a selector
		// (no argument evaluated yet)
		arg0 := call.ArgList[0]
		selx, _ := unparen(arg0).(*syntax.SelectorExpr)
		if selx == nil {
			check.errorf(arg0, BadOffsetofSyntax, invalidArg+"%s is not a selector expression", arg0)
			check.use(arg0)
			return
		}

		check.expr(nil, x, selx.X)
		if x.mode == invalid {
			return
		}

		base := derefStructPtr(x.typ)
		sel := selx.Sel.Value
		obj, index, indirect := LookupFieldOrMethod(base, false, check.pkg, sel)
		switch obj.(type) {
		case nil:
			check.errorf(x, MissingFieldOrMethod, invalidArg+"%s has no single field %s", base, sel)
			return
		case *Func:
			// TODO(gri) Using derefStructPtr may result in methods being found
			// that don't actually exist. An error either way, but the error
			// message is confusing. See: https://play.golang.org/p/al75v23kUy ,
			// but go/types reports: "invalid argument: x.m is a method value".
			check.errorf(arg0, InvalidOffsetof, invalidArg+"%s is a method value", arg0)
			return
		}
		if indirect {
			check.errorf(x, InvalidOffsetof, invalidArg+"field %s is embedded via a pointer in %s", sel, base)
			return
		}

		// TODO(gri) Should we pass x.typ instead of base (and have indirect report if derefStructPtr indirected)?
		check.recordSelection(selx, FieldVal, base, obj, index, false)

		// record the selector expression (was bug - go.dev/issue/47895)
		{
			mode := value
			if x.mode == variable || indirect {
				mode = variable
			}
			check.record(&operand{mode, selx, obj.Type(), nil, 0})
		}

		// The field offset is considered a variable even if the field is declared before
		// the part of the struct which is variable-sized. This makes both the rules
		// simpler and also permits (or at least doesn't prevent) a compiler from re-
		// arranging struct fields if it wanted to.
		if hasVarSize(base, nil) {
			x.mode = value
			if check.recordTypes() {
				check.recordBuiltinType(call.Fun, makeSig(Typ[Uintptr], obj.Type()))
			}
		} else {
			offs := check.conf.offsetof(base, index)
			if offs < 0 {
				check.errorf(x, TypeTooLarge, "%s is too large", x)
				return
			}
			x.mode = constant_
			x.val = constant.MakeInt64(offs)
			// result is constant - no need to record signature
		}
		x.typ = Typ[Uintptr]

	case _Sizeof:
		// unsafe.Sizeof(x T) uintptr
		check.assignment(x, nil, "argument to unsafe.Sizeof")
		if x.mode == invalid {
			return
		}

		if hasVarSize(x.typ, nil) {
			x.mode = value
			if check.recordTypes() {
				check.recordBuiltinType(call.Fun, makeSig(Typ[Uintptr], x.typ))
			}
		} else {
			size := check.conf.sizeof(x.typ)
			if size < 0 {
				check.errorf(x, TypeTooLarge, "%s is too large", x)
				return
			}
			x.mode = constant_
			x.val = constant.MakeInt64(size)
			// result is constant - no need to record signature
		}
		x.typ = Typ[Uintptr]

	case _Slice:
		// unsafe.Slice(ptr *T, len IntegerType) []T
		if !check.verifyVersionf(check.pkg, call.Fun, go1_17, "unsafe.Slice") {
			return
		}

		ptr, _ := under(x.typ).(*Pointer) // TODO(gri) should this be coreType rather than under?
		if ptr == nil {
			check.errorf(x, InvalidUnsafeSlice, invalidArg+"%s is not a pointer", x)
			return
		}

		var y operand
		arg(&y, 1)
		if !check.isValidIndex(&y, InvalidUnsafeSlice, "length", false) {
			return
		}

		x.mode = value
		x.typ = NewSlice(ptr.base)
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(x.typ, ptr, y.typ))
		}

	case _SliceData:
		// unsafe.SliceData(slice []T) *T
		if !check.verifyVersionf(check.pkg, call.Fun, go1_20, "unsafe.SliceData") {
			return
		}

		slice, _ := under(x.typ).(*Slice) // TODO(gri) should this be coreType rather than under?
		if slice == nil {
			check.errorf(x, InvalidUnsafeSliceData, invalidArg+"%s is not a slice", x)
			return
		}

		x.mode = value
		x.typ = NewPointer(slice.elem)
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(x.typ, slice))
		}

	case _String:
		// unsafe.String(ptr *byte, len IntegerType) string
		if !check.verifyVersionf(check.pkg, call.Fun, go1_20, "unsafe.String") {
			return
		}

		check.assignment(x, NewPointer(universeByte), "argument to unsafe.String")
		if x.mode == invalid {
			return
		}

		var y operand
		arg(&y, 1)
		if !check.isValidIndex(&y, InvalidUnsafeString, "length", false) {
			return
		}

		x.mode = value
		x.typ = Typ[String]
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(x.typ, NewPointer(universeByte), y.typ))
		}

	case _StringData:
		// unsafe.StringData(str string) *byte
		if !check.verifyVersionf(check.pkg, call.Fun, go1_20, "unsafe.StringData") {
			return
		}

		check.assignment(x, Typ[String], "argument to unsafe.StringData")
		if x.mode == invalid {
			return
		}

		x.mode = value
		x.typ = NewPointer(universeByte)
		if check.recordTypes() {
			check.recordBuiltinType(call.Fun, makeSig(x.typ, Typ[String]))
		}

	case _Assert:
		// assert(pred) causes a typechecker error if pred is false.
		// The result of assert is the value of pred if there is no error.
		// Note: assert is only available in self-test mode.
		if x.mode != constant_ || !isBoolean(x.typ) {
			check.errorf(x, Test, invalidArg+"%s is not a boolean constant", x)
			return
		}
		if x.val.Kind() != constant.Bool {
			check.errorf(x, Test, "internal error: value of %s should be a boolean constant", x)
			return
		}
		if !constant.BoolVal(x.val) {
			check.errorf(call, Test, "%v failed", call)
			// compile-time assertion failure - safe to continue
		}
		// result is constant - no need to record signature

	case _Trace:
		// trace(x, y, z, ...) dumps the positions, expressions, and
		// values of its arguments. The result of trace is the value
		// of the first argument.
		// Note: trace is only available in self-test mode.
		// (no argument evaluated yet)
		if nargs == 0 {
			check.dump("%v: trace() without arguments", atPos(call))
			x.mode = novalue
			break
		}
		var t operand
		x1 := x
		for _, arg := range call.ArgList {
			check.rawExpr(nil, x1, arg, nil, false) // permit trace for types, e.g.: new(trace(T))
			check.dump("%v: %s", atPos(x1), x1)
			x1 = &t // use incoming x only for first argument
		}
		// trace is only available in test mode - no need to record signature

	default:
		unreachable()
	}

	return true
}

// hasVarSize reports if the size of type t is variable due to type parameters
// or if the type is infinitely-sized due to a cycle for which the type has not
// yet been checked.
func hasVarSize(t Type, seen map[*Named]bool) (varSized bool) {
	// Cycles are only possible through *Named types.
	// The seen map is used to detect cycles and track
	// the results of previously seen types.
	if named, _ := t.(*Named); named != nil {
		if v, ok := seen[named]; ok {
			return v
		}
		if seen == nil {
			seen = make(map[*Named]bool)
		}
		seen[named] = true // possibly cyclic until proven otherwise
		defer func() {
			seen[named] = varSized // record final determination for named
		}()
	}

	switch u := under(t).(type) {
	case *Array:
		return hasVarSize(u.elem, seen)
	case *Struct:
		for _, f := range u.fields {
			if hasVarSize(f.typ, seen) {
				return true
			}
		}
	case *Interface:
		return isTypeParam(t)
	case *Named, *Union:
		unreachable()
	}
	return false
}

// applyTypeFunc applies f to x. If x is a type parameter,
// the result is a type parameter constrained by an new
// interface bound. The type bounds for that interface
// are computed by applying f to each of the type bounds
// of x. If any of these applications of f return nil,
// applyTypeFunc returns nil.
// If x is not a type parameter, the result is f(x).
func (check *Checker) applyTypeFunc(f func(Type) Type, x *operand, id builtinId) Type {
	if tp, _ := x.typ.(*TypeParam); tp != nil {
		// Test if t satisfies the requirements for the argument
		// type and collect possible result types at the same time.
		var terms []*Term
		if !tp.is(func(t *term) bool {
			if t == nil {
				return false
			}
			if r := f(t.typ); r != nil {
				terms = append(terms, NewTerm(t.tilde, r))
				return true
			}
			return false
		}) {
			return nil
		}

		// We can type-check this fine but we're introducing a synthetic
		// type parameter for the result. It's not clear what the API
		// implications are here. Report an error for 1.18 but continue
		// type-checking.
		var code Code
		switch id {
		case _Real:
			code = InvalidReal
		case _Imag:
			code = InvalidImag
		case _Complex:
			code = InvalidComplex
		default:
			unreachable()
		}
		check.softErrorf(x, code, "%s not supported as argument to %s for go1.18 (see go.dev/issue/50937)", x, predeclaredFuncs[id].name)

		// Construct a suitable new type parameter for the result type.
		// The type parameter is placed in the current package so export/import
		// works as expected.
		tpar := NewTypeName(nopos, check.pkg, tp.obj.name, nil)
		ptyp := check.newTypeParam(tpar, NewInterfaceType(nil, []Type{NewUnion(terms)})) // assigns type to tpar as a side-effect
		ptyp.index = tp.index

		return ptyp
	}

	return f(x.typ)
}

// makeSig makes a signature for the given argument and result types.
// Default types are used for untyped arguments, and res may be nil.
func makeSig(res Type, args ...Type) *Signature {
	list := make([]*Var, len(args))
	for i, param := range args {
		list[i] = NewVar(nopos, nil, "", Default(param))
	}
	params := NewTuple(list...)
	var result *Tuple
	if res != nil {
		assert(!isUntyped(res))
		result = NewTuple(NewVar(nopos, nil, "", res))
	}
	return &Signature{params: params, results: result}
}

// arrayPtrDeref returns A if typ is of the form *A and A is an array;
// otherwise it returns typ.
func arrayPtrDeref(typ Type) Type {
	if p, ok := typ.(*Pointer); ok {
		if a, _ := under(p.base).(*Array); a != nil {
			return a
		}
	}
	return typ
}

// unparen returns e with any enclosing parentheses stripped.
func unparen(e syntax.Expr) syntax.Expr {
	for {
		p, ok := e.(*syntax.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}
