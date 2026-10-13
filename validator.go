package validator

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"unsafe"
)

// per validate construct
type validate struct {
	v              *Validate
	top            reflect.Value
	ns             []byte
	actualNs       []byte
	errs           ValidationErrors
	includeExclude map[string]struct{} // reset only if StructPartial or StructExcept are called, no need otherwise
	ffn            FilterFunc
	slflParent     reflect.Value // StructLevel & FieldLevel
	slCurrent      reflect.Value // StructLevel & FieldLevel
	flField        reflect.Value // StructLevel & FieldLevel
	cf             *cField       // StructLevel & FieldLevel
	ct             *cTag         // StructLevel & FieldLevel
	misc           []byte        // misc reusable
	str1           string        // misc reusable
	str2           string        // misc reusable
	fldIsPointer   bool          // StructLevel & FieldLevel
	isPartial      bool
	hasExcludes    bool

	// 按调用生效的观察状态：仅 StructCtxObserved / VarCtxObserved 设置，
	// 随 sync.Pool 取用而隔离，并发调用与重入调用之间互不影响。
	observe    ObserveFunc     // 当前调用的观察函数，nil 表示不观察
	obsCtx     context.Context // 结构级上报回调（无 ctx 参数）使用的上下文
	observeErr error           // 观察函数返回的首个错误，非 nil 后遍历立即停止
	seq        uint64          // 当前调用内已发送的事件序号
}

// parent and current will be the same the first run of validateStruct
func (v *validate) validateStruct(ctx context.Context, parent reflect.Value, current reflect.Value, typ reflect.Type, ns []byte, structNs []byte, ct *cTag) {
	cs, ok := v.v.structCache.Get(typ)
	if !ok {
		cs = v.v.extractStructCache(current, typ.Name())
	}

	if len(ns) == 0 && len(cs.name) != 0 {
		ns = append(ns, cs.name...)
		ns = append(ns, '.')

		structNs = append(structNs, cs.name...)
		structNs = append(structNs, '.')
	}

	// ct is nil on top level struct, and structs as fields that have no tag info
	// so if nil or if not nil and the structonly tag isn't present
	if ct == nil || ct.typeof != typeStructOnly {
		var f *cField

		for i := 0; i < len(cs.fields); i++ {
			// 观察函数要求中止时，不再遍历剩余字段
			if v.observeErr != nil {
				return
			}

			f = cs.fields[i]

			if v.isPartial {
				if v.ffn != nil {
					// used with StructFiltered
					if v.ffn(append(structNs, f.name...)) {
						continue
					}
				} else {
					// used with StructPartial & StructExcept
					_, ok = v.includeExclude[string(append(structNs, f.name...))]

					if (ok && v.hasExcludes) || (!ok && !v.hasExcludes) {
						continue
					}
				}
			}

			v.traverseField(ctx, current, current.Field(f.idx), ns, structNs, f, f.cTags)
		}
	}

	// check if any struct level validations, after all field validations already checked.
	// first iteration will have no info about nostructlevel tag, and is checked prior to
	// calling the next iteration of validateStruct called from traverseField.
	// 字段遍历期间观察函数可能已经要求中止，此时结构级验证同样不再执行
	if cs.fn != nil && v.observeErr == nil {
		v.slflParent = parent
		v.slCurrent = current
		v.ns = ns
		v.actualNs = structNs
		v.obsCtx = ctx

		cs.fn(ctx, v)
	}
}

// traverseField validates any field, be it a struct or single field, ensures it's validity and passes it along to be validated via it's tag options
func (v *validate) traverseField(ctx context.Context, parent reflect.Value, current reflect.Value, ns []byte, structNs []byte, cf *cField, ct *cTag) {
	var typ reflect.Type
	var kind reflect.Kind

	current, kind, v.fldIsPointer = v.extractTypeInternal(current, false)

	var isNestedStruct bool

	switch kind {
	case reflect.Ptr, reflect.Interface, reflect.Invalid:

		if ct == nil {
			return
		}

		if ct.typeof == typeOmitEmpty || ct.typeof == typeIsDefault {
			return
		}

		if ct.typeof == typeOmitNil && (kind != reflect.Invalid && current.IsNil()) {
			return
		}

		if ct.typeof == typeOmitZero {
			return
		}

		if ct.hasTag {
			if kind == reflect.Invalid {
				v.str1 = appendAltName(ns, cf.altName)
				if v.v.hasTagNameFunc {
					v.str2 = string(append(structNs, cf.name...))
				} else {
					v.str2 = v.str1
				}
				fe := &fieldError{
					v:              v.v,
					tag:            ct.aliasTag,
					actualTag:      ct.tag,
					ns:             v.str1,
					structNs:       v.str2,
					fieldLen:       uint8(len(cf.altName)),
					structfieldLen: uint8(len(cf.name)),
					param:          ct.param,
					kind:           kind,
				}
				v.errs = append(v.errs, fe)
				// 无类型 nil 被判定为当前规则失败（规则函数本身不会执行）
				v.observeRule(ctx, ns, structNs, cf, ct, nil, false, fe)
				return
			}

			v.str1 = appendAltName(ns, cf.altName)
			if v.v.hasTagNameFunc {
				v.str2 = string(append(structNs, cf.name...))
			} else {
				v.str2 = v.str1
			}
			if !ct.runValidationWhenNil {
				fe := &fieldError{
					v:              v.v,
					tag:            ct.aliasTag,
					actualTag:      ct.tag,
					ns:             v.str1,
					structNs:       v.str2,
					fieldLen:       uint8(len(cf.altName)),
					structfieldLen: uint8(len(cf.name)),
					value:          getValue(current),
					param:          ct.param,
					kind:           kind,
					typ:            current.Type(),
				}
				v.errs = append(v.errs, fe)
				// nil 指针/接口且规则不允许在 nil 上执行时，当前规则直接失败
				v.observeRule(ctx, ns, structNs, cf, ct, current.Type(), false, fe)
				return
			}
		}

		if kind == reflect.Invalid {
			return
		}

	case reflect.Struct:
		isNestedStruct = !current.Type().ConvertibleTo(timeType)
		// For backward compatibility before struct level validation tags were supported
		// as there were a number of projects relying on `required` not failing on non-pointer
		// structs. Since it's basically nonsensical to use `required` with a non-pointer struct
		// are explicitly skipping the required validation for it. This WILL be removed in the
		// next major version.
		if isNestedStruct && !v.v.requiredStructEnabled && ct != nil && ct.tag == requiredTag {
			ct = ct.next
		}
	}

	typ = current.Type()

OUTER:
	for {
		// 观察函数返回错误后，规则链不再继续执行
		if v.observeErr != nil {
			return
		}

		if ct == nil || !ct.hasTag || (isNestedStruct && len(cf.name) == 0) {
			// isNestedStruct check here
			if isNestedStruct {
				// if len == 0 then validating using 'Var' or 'VarWithValue'
				// Var - doesn't make much sense to do it that way, should call 'Struct', but no harm...
				// VarWithField - this allows for validating against each field within the struct against a specific value
				//                pretty handy in certain situations
				if len(cf.name) > 0 {
					if len(cf.altName) > 0 {
						ns = append(append(ns, cf.altName...), '.')
					}
					structNs = append(append(structNs, cf.name...), '.')
				}

				v.validateStruct(ctx, parent, current, typ, ns, structNs, ct)
			}
			return
		}

		switch ct.typeof {
		case typeNoStructLevel:
			return

		case typeStructOnly:
			if isNestedStruct {
				// if len == 0 then validating using 'Var' or 'VarWithValue'
				// Var - doesn't make much sense to do it that way, should call 'Struct', but no harm...
				// VarWithField - this allows for validating against each field within the struct against a specific value
				//                pretty handy in certain situations
				if len(cf.name) > 0 {
					if len(cf.altName) > 0 {
						ns = append(append(ns, cf.altName...), '.')
					}
					structNs = append(append(structNs, cf.name...), '.')
				}

				v.validateStruct(ctx, parent, current, typ, ns, structNs, ct)
			}
			return

		case typeOmitEmpty:

			// set Field Level fields
			v.slflParent = parent
			v.flField = current
			v.cf = cf
			v.ct = ct

			if !hasValue(v) {
				return
			}

			ct = ct.next
			continue

		case typeOmitZero:
			v.slflParent = parent
			v.flField = current
			v.cf = cf
			v.ct = ct

			if !hasNotZeroValue(v) {
				return
			}

			ct = ct.next
			continue

		case typeOmitNil:
			v.slflParent = parent
			v.flField = current
			v.cf = cf
			v.ct = ct

			switch field := v.Field(); field.Kind() {
			case reflect.Slice, reflect.Map, reflect.Ptr, reflect.Interface, reflect.Chan, reflect.Func:
				if field.IsNil() {
					return
				}
			default:
				if v.fldIsPointer && getValue(field) == nil {
					return
				}
			}

			ct = ct.next
			continue

		case typeEndKeys:
			return

		case typeDive:

			ct = ct.next

			// traverse slice or map here
			// or panic ;)
			switch kind {
			case reflect.Slice, reflect.Array:

				var i64 int64
				reusableCF := &cField{}

				for i := 0; i < current.Len(); i++ {
					i64 = int64(i)

					v.misc = append(v.misc[0:0], cf.name...)
					v.misc = append(v.misc, '[')
					v.misc = strconv.AppendInt(v.misc, i64, 10)
					v.misc = append(v.misc, ']')

					reusableCF.name = string(v.misc)

					if cf.namesEqual {
						reusableCF.altName = reusableCF.name
					} else {
						v.misc = append(v.misc[0:0], cf.altName...)
						v.misc = append(v.misc, '[')
						v.misc = strconv.AppendInt(v.misc, i64, 10)
						v.misc = append(v.misc, ']')

						reusableCF.altName = string(v.misc)
					}
					v.traverseField(ctx, parent, current.Index(i), ns, structNs, reusableCF, ct)

					// 观察中止后不再遍历剩余元素
					if v.observeErr != nil {
						return
					}
				}

			case reflect.Map:

				var pv string
				reusableCF := &cField{}

				for _, key := range current.MapKeys() {
					pv = fmt.Sprintf("%v", key)

					v.misc = append(v.misc[0:0], cf.name...)
					v.misc = append(v.misc, '[')
					v.misc = append(v.misc, pv...)
					v.misc = append(v.misc, ']')

					reusableCF.name = string(v.misc)

					if cf.namesEqual {
						reusableCF.altName = reusableCF.name
					} else {
						v.misc = append(v.misc[0:0], cf.altName...)
						v.misc = append(v.misc, '[')
						v.misc = append(v.misc, pv...)
						v.misc = append(v.misc, ']')

						reusableCF.altName = string(v.misc)
					}

					if ct != nil && ct.typeof == typeKeys && ct.keys != nil {
						v.traverseField(ctx, parent, key, ns, structNs, reusableCF, ct.keys)
						if v.observeErr != nil {
							return
						}
						// can be nil when just keys being validated
						if ct.next != nil {
							v.traverseField(ctx, parent, current.MapIndex(key), ns, structNs, reusableCF, ct.next)
						} else {
							// Struct fallback when map values are structs
							val := current.MapIndex(key)
							switch val.Kind() {
							case reflect.Ptr:
								if val.Elem().Kind() == reflect.Struct {
									// Dive into the struct so its own tags run
									v.traverseField(ctx, parent, val, ns, structNs, reusableCF, nil)
								}
							case reflect.Struct:
								v.traverseField(ctx, parent, val, ns, structNs, reusableCF, nil)
							}
						}
					} else {
						v.traverseField(ctx, parent, current.MapIndex(key), ns, structNs, reusableCF, ct)
					}

					// 观察中止后不再遍历剩余键值对
					if v.observeErr != nil {
						return
					}
				}

			default:
				// throw error, if not a slice or map then should not have gotten here
				// bad dive tag
				panic("dive error! can't dive on a non slice or map")
			}

			return

		case typeOr:

			v.misc = v.misc[0:0]

			for {
				// set Field Level fields
				v.slflParent = parent
				v.flField = current
				v.cf = cf
				v.ct = ct

				if ct.fn(ctx, v) {
					// or 分支实际执行且通过；后续未执行的分支仅被排空，不产生事件
					v.observeRule(ctx, ns, structNs, cf, ct, typ, true, nil)
					if v.observeErr != nil {
						return
					}

					if ct.isBlockEnd {
						ct = ct.next
						continue OUTER
					}

					// drain rest of the 'or' values, then continue or leave
					for {
						ct = ct.next

						if ct == nil {
							continue OUTER
						}

						if ct.typeof != typeOr {
							continue OUTER
						}

						if ct.isBlockEnd {
							ct = ct.next
							continue OUTER
						}
					}
				}

				// 该 or 分支执行失败：若不是最后一个分支，先发送一条不带 FieldError
				// 的失败事件；整体失败的聚合错误随最后一个分支的事件上报。
				if !ct.isBlockEnd && ct.next != nil {
					v.observeRule(ctx, ns, structNs, cf, ct, typ, false, nil)
					if v.observeErr != nil {
						return
					}
				}

				v.misc = append(v.misc, '|')
				v.misc = append(v.misc, ct.tag...)

				if ct.hasParam {
					v.misc = append(v.misc, '=')
					v.misc = append(v.misc, ct.param...)
				}

				if ct.isBlockEnd || ct.next == nil {
					// if we get here, no valid 'or' value and no more tags
					v.str1 = appendAltName(ns, cf.altName)

					if v.v.hasTagNameFunc {
						v.str2 = string(append(structNs, cf.name...))
					} else {
						v.str2 = v.str1
					}

					var fe *fieldError

					if ct.hasAlias {
						fe = &fieldError{
							v:              v.v,
							tag:            ct.aliasTag,
							actualTag:      ct.actualAliasTag,
							ns:             v.str1,
							structNs:       v.str2,
							fieldLen:       uint8(len(cf.altName)),
							structfieldLen: uint8(len(cf.name)),
							value:          getValue(current),
							param:          ct.param,
							kind:           kind,
							typ:            typ,
						}
					} else {
						tVal := string(v.misc)[1:]

						fe = &fieldError{
							v:              v.v,
							tag:            tVal,
							actualTag:      tVal,
							ns:             v.str1,
							structNs:       v.str2,
							fieldLen:       uint8(len(cf.altName)),
							structfieldLen: uint8(len(cf.name)),
							value:          getValue(current),
							param:          ct.param,
							kind:           kind,
							typ:            typ,
						}
					}

					v.errs = append(v.errs, fe)
					// 聚合 FieldError 附着在最后一个实际执行的失败分支事件上
					v.observeRule(ctx, ns, structNs, cf, ct, typ, false, fe)
					return
				}

				ct = ct.next
			}

		default:

			// set Field Level fields
			v.slflParent = parent
			v.flField = current
			v.cf = cf
			v.ct = ct

			passed := ct.fn(ctx, v)

			var fe *fieldError

			if !passed {
				v.str1 = appendAltName(ns, cf.altName)

				if v.v.hasTagNameFunc {
					v.str2 = string(append(structNs, cf.name...))
				} else {
					v.str2 = v.str1
				}

				fe = &fieldError{
					v:              v.v,
					tag:            ct.aliasTag,
					actualTag:      ct.tag,
					ns:             v.str1,
					structNs:       v.str2,
					fieldLen:       uint8(len(cf.altName)),
					structfieldLen: uint8(len(cf.name)),
					value:          getValue(current),
					param:          ct.param,
					kind:           kind,
					typ:            typ,
				}

				v.errs = append(v.errs, fe)
			}

			// 每个实际执行的字段规则（含自定义验证函数）都在此发出事件
			v.observeRule(ctx, ns, structNs, cf, ct, typ, passed, fe)

			if v.observeErr != nil || !passed {
				return
			}

			ct = ct.next
		}
	}
}

func appendAltName(ns []byte, altName string) string {
	if len(altName) > 0 {
		return string(append(ns, altName...))
	}
	if n := len(ns); n > 0 && ns[n-1] == '.' {
		return string(ns[:n-1])
	}
	return string(ns)
}

func getValue(val reflect.Value) interface{} {
	if val.CanInterface() {
		return val.Interface()
	}

	if val.CanAddr() {
		return reflect.NewAt(val.Type(), unsafe.Pointer(val.UnsafeAddr())).Elem().Interface()
	}

	switch val.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return val.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return val.Uint()
	case reflect.Complex64, reflect.Complex128:
		return val.Complex()
	case reflect.Float32, reflect.Float64:
		return val.Float()
	default:
		return val.String()
	}
}
