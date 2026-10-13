package validator

import (
	"context"
	"reflect"
)

// ObserveFunc 是按验证调用注册的观察回调。
//
// 验证过程中每一个实际执行的字段规则都会按照真实遍历顺序同步回调一次，
// 结构级验证通过 StructLevel.ReportError / ReportValidationErrors 主动上报的
// 每条错误也会分别回调一次。回调中收到的 ValidationEvent 内容均为调用时刻的
// 稳定快照，可以在回调返回之后继续保留和访问。
//
// 回调返回非 nil 错误时，当前验证调用会立即停止，并由触发观察的入口方法
// （StructCtxObserved 或 VarCtxObserved）原样返回该错误；此前已经发出的事件
// 顺序保持不变。
//
// 回调可以重新进入同一个 *Validate 发起新的验证调用；重入调用拥有各自独立的
// 观察状态（序号、中止错误等），不会与外层调用相互影响。
type ObserveFunc func(ctx context.Context, event *ValidationEvent) error

// ValidationEvent 描述一次字段规则的实际执行结果。
type ValidationEvent struct {
	// Seq 是当前验证调用内连续递增的事件序号，从 1 开始；
	// 重入验证器产生的嵌套调用拥有独立的序号序列。
	Seq uint64

	// Namespace 是对外字段路径，字段名优先取 RegisterTagNameFunc 提供的名称，
	// 例如 "User.FirstName" 或 JSON 名称 "user.first_name"。
	Namespace string

	// StructNamespace 是使用 Go 结构字段名拼接的字段路径，
	// 例如 "User.FirstName"。
	StructNamespace string

	// Tag 是实际写入规则时使用的标签；当规则来自别名时，这里为别名名称，
	// 例如别名 iscolor 的值为 "iscolor"。
	Tag string

	// ActualTag 是别名展开之后真正执行的标签，例如别名 iscolor 下实际失败的
	// "hexcolor"；or 表达式整体失败时为形如 "hexcolor|rgb|rgba" 的组合标签。
	ActualTag string

	// Param 是规则参数（标签中 '=' 之后的部分），没有参数时为空字符串。
	Param string

	// Type 是被验证值在指针、接口及自定义类型解包之后的反射类型；
	// 值为无类型 nil 时为 nil。
	Type reflect.Type

	// Pass 表示该条规则本次执行是否通过。
	Pass bool

	// FieldError 是规则失败时产生的 FieldError，规则通过时为 nil；
	// 结构级主动上报产生的事件中同样携带对应的 FieldError。
	FieldError FieldError
}

// observeRule 是热路径上的薄蹦床：仅做观察者是否存在的判定，保持可内联，
// 使未观察的既有入口不承担函数调用与事件构造开销；真正的事件组装与回调放在
// observeRuleEmit 中。
//
// ns、structNs 为当前字段所在层级的路径前缀，cf、ct 分别描述当前字段与规则，
// typ 为解包后的反射类型；规则失败时 fe 为已追加到错误集合中的 fieldError。
func (v *validate) observeRule(ctx context.Context, ns, structNs []byte, cf *cField, ct *cTag, typ reflect.Type, passed bool, fe *fieldError) {
	// 未注册观察者，或观察函数已经要求中止时，均不再产生事件
	if v.observe == nil || v.observeErr != nil {
		return
	}
	v.observeRuleEmit(ctx, ns, structNs, cf, ct, typ, passed, fe)
}

// observeRuleEmit 组装并发送一条字段规则事件，仅在当前调用注册了观察者时执行。
func (v *validate) observeRuleEmit(ctx context.Context, ns, structNs []byte, cf *cField, ct *cTag, typ reflect.Type, passed bool, fe *fieldError) {
	ext := appendAltName(ns, cf.altName)

	var sns string
	if v.v.hasTagNameFunc {
		sns = string(append(structNs, cf.name...))
	} else {
		sns = ext
	}

	// 规则对外标签：别名展开的规则取别名，其余取规则自身标签。
	// 注意不能直接用 ct.aliasTag —— 解析 or 链时解析器会把前一个分支节点的
	// aliasTag 覆盖成后一个分支名，普通规则的 aliasTag 与 tag 相同，
	// 因此以 hasAlias 判定才能拿到每个分支真实的标签。
	tag, actualTag, param := ct.tag, ct.tag, ct.param
	if ct.hasAlias {
		tag = ct.aliasTag
	}
	// or 表达式整体失败时，事件携带的聚合 FieldError 上的标签才是对外报告值。
	if fe != nil {
		tag = fe.tag
		actualTag = fe.actualTag
		param = fe.param
	}

	v.seq++

	ev := &ValidationEvent{
		Seq:             v.seq,
		Namespace:       ext,
		StructNamespace: sns,
		Tag:             tag,
		ActualTag:       actualTag,
		Param:           param,
		Type:            typ,
		Pass:            passed,
	}

	// 注意：只能在非 nil 时赋给接口字段，否则会产生 typed-nil 接口
	if fe != nil {
		ev.FieldError = fe
	}

	if err := v.observe(ctx, ev); err != nil {
		v.observeErr = err
	}
}

// observeReported 是结构级上报路径上的薄蹦床，判定后转入 observeReportedEmit。
func (v *validate) observeReported(fe *fieldError) {
	if v.observe == nil || v.observeErr != nil {
		return
	}
	v.observeReportedEmit(fe)
}

// observeReportedEmit 为结构级验证函数主动上报的一条 FieldError 发送事件。
// 事件内容完全取自上报的 fieldError，序号紧跟在当前调用已有事件之后。
func (v *validate) observeReportedEmit(fe *fieldError) {
	v.seq++

	ev := &ValidationEvent{
		Seq:             v.seq,
		Namespace:       fe.ns,
		StructNamespace: fe.structNs,
		Tag:             fe.tag,
		ActualTag:       fe.actualTag,
		Param:           fe.param,
		Type:            fe.typ,
		Pass:            false,
	}
	ev.FieldError = fe

	if err := v.observe(v.obsCtx, ev); err != nil {
		v.observeErr = err
	}
}

// finishObserved 汇总一次被观察调用的返回结果：观察函数返回错误时原样返回该
// 错误（丢弃累积的验证错误），否则与既有入口一样在存在错误时返回
// ValidationErrors。返回前会清空全部按调用隔离的观察状态与残留错误切片，
// 确保 validate 归还 sync.Pool 后不会污染后续调用。
func (v *validate) finishObserved() error {
	var err error

	switch {
	case v.observeErr != nil:
		err = v.observeErr
	case len(v.errs) > 0:
		err = v.errs
	}

	// 事件中的 FieldError 由已发出的事件自身持有，这里可以安全丢弃切片
	v.errs = nil
	v.observe = nil
	v.obsCtx = nil
	v.observeErr = nil
	v.seq = 0

	return err
}
