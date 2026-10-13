package validator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/go-playground/assert/v2"
)

// ---- 观察能力测试公共工具 ----

type oevCollector struct {
	events []*ValidationEvent
}

func (c *oevCollector) observe(_ context.Context, ev *ValidationEvent) error {
	c.events = append(c.events, ev)
	return nil
}

func (c *oevCollector) tags() []string {
	out := make([]string, len(c.events))
	for i, ev := range c.events {
		out[i] = ev.Namespace + "#" + ev.Tag + "/" + ev.ActualTag
	}
	return out
}

func oevFindEvent(t *testing.T, evs []*ValidationEvent, namespace, tag string) *ValidationEvent {
	t.Helper()
	for _, ev := range evs {
		if ev.Namespace == namespace && ev.Tag == tag {
			return ev
		}
	}
	t.Fatalf("event not found: ns=%q tag=%q", namespace, tag)
	return nil
}

func oevAssertContiguousSeq(t *testing.T, evs []*ValidationEvent) {
	t.Helper()
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("seq not contiguous at %d: got %d", i, ev.Seq)
		}
	}
}

// go-playground/assert v2 对 reflect.Type 的比较不可靠，这里直接用 == 比较
func oevAssertType(t *testing.T, got, want reflect.Type) {
	t.Helper()
	if got != want {
		t.Fatalf("reflect.Type = %v, want %v", got, want)
	}
}

// ---- 基础遍历：嵌套结构、slice dive、空值跳过、nil 指针、序号与错误对应 ----

type oevBasicInner struct {
	Street string `validate:"required,min=3"`
}

type oevBasicOuter struct {
	Name  string `validate:"required,min=2"`
	Inner oevBasicInner
	Items []string `validate:"required,dive,required,min=1"`
	Skip  string   `validate:"omitempty,min=3"`
	Ptr   *string  `validate:"required"`
}

func TestObservedStructBasicTraversal(t *testing.T) {
	v := New()
	c := new(oevCollector)

	s := &oevBasicOuter{
		Name:  "a",                         // required 通过、min=2 失败
		Inner: oevBasicInner{Street: "ab"}, // required 通过、min=3 失败
		Items: []string{"ok", ""},          // [0] 全过，[1] required 失败后短路
		Skip:  "",                          // omitempty 命中，后续 min=3 不执行
		Ptr:   nil,                         // required 在 nil 指针上直接失败
	}

	err := v.StructCtxObserved(context.Background(), s, c.observe)
	verrs, ok := err.(ValidationErrors)
	assert.Equal(t, ok, true)
	assert.Equal(t, len(verrs), 4)

	oevAssertContiguousSeq(t, c.events)
	assert.Equal(t, len(c.events), 9)

	// 规则按真实遍历顺序送达，控制标签（dive/omitempty）不产生事件
	assert.Equal(t, c.tags(), []string{
		"oevBasicOuter.Name#required/required",
		"oevBasicOuter.Name#min/min",
		"oevBasicOuter.Inner.Street#required/required",
		"oevBasicOuter.Inner.Street#min/min",
		"oevBasicOuter.Items#required/required",
		"oevBasicOuter.Items[0]#required/required",
		"oevBasicOuter.Items[0]#min/min",
		"oevBasicOuter.Items[1]#required/required",
		// Skip 字段 omitempty 命中，没有任何事件
		"oevBasicOuter.Ptr#required/required",
	})

	// 通过/失败状态与参数、反射类型
	passRequired := oevFindEvent(t, c.events, "oevBasicOuter.Name", "required")
	assert.Equal(t, passRequired.Pass, true)
	assert.Equal(t, passRequired.Param, "")
	assert.Equal(t, passRequired.FieldError, nil)
	oevAssertType(t, passRequired.Type, reflect.TypeOf(""))

	failMin := oevFindEvent(t, c.events, "oevBasicOuter.Name", "min")
	assert.Equal(t, failMin.Pass, false)
	assert.Equal(t, failMin.Param, "2")
	oevAssertType(t, failMin.Type, reflect.TypeOf(""))
	if failMin.FieldError == nil {
		t.Fatal("failed event must carry FieldError")
	}
	assert.Equal(t, failMin.FieldError.Tag(), "min")
	assert.Equal(t, failMin.FieldError.Namespace(), "oevBasicOuter.Name")

	// [1] required 失败后短路，min=1 未执行
	for _, ev := range c.events {
		if ev.Namespace == "oevBasicOuter.Items[1]" && ev.Tag == "min" {
			t.Fatal("min must be short-circuited after required failure")
		}
	}

	// nil 指针合成失败：规则函数未执行，但失败事件带 Kind=Ptr 的 FieldError
	ptrEv := oevFindEvent(t, c.events, "oevBasicOuter.Ptr", "required")
	assert.Equal(t, ptrEv.Pass, false)
	oevAssertType(t, ptrEv.Type, reflect.TypeOf((*string)(nil)))
	if ptrEv.FieldError == nil {
		t.Fatal("nil pointer event must carry FieldError")
	}
	assert.Equal(t, ptrEv.FieldError.Kind(), reflect.Ptr)

	// 所有返回的 ValidationErrors 与带 FieldError 的失败事件一一对应、顺序一致
	feEvents := make([]FieldError, 0, len(verrs))
	for _, ev := range c.events {
		if !ev.Pass && ev.FieldError != nil {
			feEvents = append(feEvents, ev.FieldError)
		}
	}
	assert.Equal(t, len(feEvents), len(verrs))
	for i := range verrs {
		assert.Equal(t, feEvents[i], verrs[i])
		assert.Equal(t, feEvents[i].Namespace(), verrs[i].Namespace())
	}

	// 对外路径与结构路径一致（未注册 TagNameFunc）
	for _, ev := range c.events {
		assert.Equal(t, ev.Namespace, ev.StructNamespace)
	}
}

// ---- map dive + keys/endkeys：键规则先于值规则 ----

type oevMapOuter struct {
	Meta map[string]string `validate:"dive,keys,min=2,endkeys,required"`
}

func TestObservedMapDiveKeys(t *testing.T) {
	v := New()
	c := new(oevCollector)

	s := oevMapOuter{
		Meta: map[string]string{
			"x":  "v", // 键 min=2 失败，值 required 通过
			"kk": "",  // 键通过，值 required 失败
		},
	}

	err := v.StructCtxObserved(context.Background(), s, c.observe)
	verrs, ok := err.(ValidationErrors)
	assert.Equal(t, ok, true)
	assert.Equal(t, len(verrs), 2)
	oevAssertContiguousSeq(t, c.events)
	assert.Equal(t, len(c.events), 4)

	idx := func(ns, tag string) int {
		for i, ev := range c.events {
			if ev.Namespace == ns && ev.Tag == tag {
				return i
			}
		}
		return -1
	}

	// 同一个键：键规则事件必须在值规则事件之前
	badKey := idx("oevMapOuter.Meta[x]", "min")
	goodVal := idx("oevMapOuter.Meta[x]", "required")
	assert.Equal(t, badKey >= 0, true)
	assert.Equal(t, goodVal >= 0, true)
	assert.Equal(t, badKey < goodVal, true)

	badKeyEv := c.events[badKey]
	assert.Equal(t, badKeyEv.Pass, false)
	assert.Equal(t, badKeyEv.Param, "2")
	// 键规则失败属于普通规则失败，携带键路径上的 FieldError
	assert.NotEqual(t, badKeyEv.FieldError, nil)
	assert.Equal(t, badKeyEv.FieldError.Namespace(), "oevMapOuter.Meta[x]")

	emptyValEv := oevFindEvent(t, c.events, "oevMapOuter.Meta[kk]", "required")
	assert.Equal(t, emptyValEv.Pass, false)
	if emptyValEv.FieldError == nil || emptyValEv.FieldError.Namespace() != "oevMapOuter.Meta[kk]" {
		t.Fatal("map value failure must carry namespaced FieldError")
	}
}

// ---- 自定义字段名（RegisterTagNameFunc） ----

type oevJSONInner struct {
	Street string `validate:"required,min=3" json:"street"`
}

type oevJSONOuter struct {
	Name  string       `validate:"min=2" json:"name"`
	Inner oevJSONInner `json:"inner"`
	Items []string     `validate:"dive,required,min=1" json:"items"`
}

func TestObservedCustomFieldNames(t *testing.T) {
	v := New()
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := fld.Tag.Get("json")
		if name == "-" || name == "" {
			return fld.Name
		}
		return name
	})
	c := new(oevCollector)

	s := oevJSONOuter{
		Name:  "x",
		Inner: oevJSONInner{Street: "ab"},
		Items: []string{"", "ok"},
	}
	err := v.StructCtxObserved(context.Background(), s, c.observe)
	assert.NotEqual(t, err, nil)
	oevAssertContiguousSeq(t, c.events)

	nameEv := oevFindEvent(t, c.events, "oevJSONOuter.name", "min")
	assert.Equal(t, nameEv.StructNamespace, "oevJSONOuter.Name")
	assert.Equal(t, nameEv.FieldError.Namespace(), "oevJSONOuter.name")
	assert.Equal(t, nameEv.FieldError.StructNamespace(), "oevJSONOuter.Name")

	streetEv := oevFindEvent(t, c.events, "oevJSONOuter.inner.street", "min")
	assert.Equal(t, streetEv.StructNamespace, "oevJSONOuter.Inner.Street")

	// dive 元素同样区分对外名与结构名
	idxEv := oevFindEvent(t, c.events, "oevJSONOuter.items[0]", "required")
	assert.Equal(t, idxEv.StructNamespace, "oevJSONOuter.Items[0]")
	assert.Equal(t, idxEv.Pass, false)
}

// ---- 别名：Tag 为别名，ActualTag 为展开后的真实标签 ----

func TestObservedAliasExpansion(t *testing.T) {
	v := New()
	v.RegisterAlias("oev_bounded", "min=2,max=5")

	// 失败：首个规则即短路，事件 Tag 为别名、ActualTag 为 min
	c1 := new(oevCollector)
	err := v.VarCtxObserved(context.Background(), "a", "oev_bounded", c1.observe)
	assert.NotEqual(t, err, nil)
	assert.Equal(t, len(c1.events), 1)
	ev := c1.events[0]
	assert.Equal(t, ev.Tag, "oev_bounded")
	assert.Equal(t, ev.ActualTag, "min")
	assert.Equal(t, ev.Param, "2")
	assert.Equal(t, ev.Pass, false)
	assert.Equal(t, ev.FieldError.Tag(), "oev_bounded")
	assert.Equal(t, ev.FieldError.ActualTag(), "min")

	// 通过：展开后的每条规则都送达
	c2 := new(oevCollector)
	err = v.VarCtxObserved(context.Background(), "abc", "oev_bounded", c2.observe)
	assert.Equal(t, err, nil)
	oevAssertContiguousSeq(t, c2.events)
	assert.Equal(t, len(c2.events), 2)
	assert.Equal(t, c2.events[0].Tag, "oev_bounded")
	assert.Equal(t, c2.events[0].ActualTag, "min")
	assert.Equal(t, c2.events[0].Pass, true)
	assert.Equal(t, c2.events[1].ActualTag, "max")
	assert.Equal(t, c2.events[1].Param, "5")
	assert.Equal(t, c2.events[1].Pass, true)
}

// ---- or 表达式：每个实际执行的分支一个事件，聚合错误挂在最后一个分支 ----

func TestObservedOrOperator(t *testing.T) {
	v := New()

	// 全失败：rgb 失败（无 FieldError）→ rgba 失败并携带聚合 FieldError
	c1 := new(oevCollector)
	err := v.VarCtxObserved(context.Background(), "this ain't right", "rgb|rgba", c1.observe)
	verrs, ok := err.(ValidationErrors)
	assert.Equal(t, ok, true)
	assert.Equal(t, len(verrs), 1)
	assert.Equal(t, len(c1.events), 2)
	oevAssertContiguousSeq(t, c1.events)

	assert.Equal(t, c1.events[0].Tag, "rgb")
	assert.Equal(t, c1.events[0].ActualTag, "rgb")
	assert.Equal(t, c1.events[0].Pass, false)
	assert.Equal(t, c1.events[0].FieldError, nil)

	last := c1.events[1]
	assert.Equal(t, last.Tag, "rgb|rgba")
	assert.Equal(t, last.ActualTag, "rgb|rgba")
	assert.Equal(t, last.Pass, false)
	assert.Equal(t, last.FieldError, verrs[0])

	// 首个分支失败、末分支通过：排空的分支不执行、不产生事件
	c2 := new(oevCollector)
	err = v.VarCtxObserved(context.Background(), "rgba(0,31,255,0.5)", "rgb|rgba", c2.observe)
	assert.Equal(t, err, nil)
	assert.Equal(t, len(c2.events), 2)
	assert.Equal(t, c2.events[0].ActualTag, "rgb")
	assert.Equal(t, c2.events[0].Pass, false)
	assert.Equal(t, c2.events[0].FieldError, nil)
	assert.Equal(t, c2.events[1].ActualTag, "rgba")
	assert.Equal(t, c2.events[1].Pass, true)

	// 别名 + or：每个执行的分支都以别名为 Tag，末事件 ActualTag 为完整展开表达式
	c3 := new(oevCollector)
	err = v.VarCtxObserved(context.Background(), "zzz", "iscolor", c3.observe)
	assert.NotEqual(t, err, nil)
	assert.Equal(t, len(c3.events), 6)
	assert.Equal(t, c3.events[0].Tag, "iscolor")
	assert.Equal(t, c3.events[0].ActualTag, "hexcolor")
	assert.Equal(t, c3.events[0].FieldError, nil)
	last3 := c3.events[5]
	assert.Equal(t, last3.Tag, "iscolor")
	assert.Equal(t, last3.ActualTag, "hexcolor|rgb|rgba|hsl|hsla|cmyk")
	assert.NotEqual(t, last3.FieldError, nil)
}

// ---- 自定义验证函数（Func 与 FuncCtx）与上下文透传 ----

type oevCtxKey struct{}

func TestObservedCustomValidationFunc(t *testing.T) {
	v := New()
	assert.Equal(t, v.RegisterValidation("oev_even", func(fl FieldLevel) bool {
		return fl.Field().Int()%2 == 0
	}), nil)

	ctx := context.WithValue(context.Background(), oevCtxKey{}, "marker")

	c := new(oevCollector)
	// 同时注册一个读取 ctx 的自定义规则，验证 ctx 原样透传到规则与观察函数
	assert.Equal(t, v.RegisterValidationCtx("oev_ctxmark", func(ctx context.Context, fl FieldLevel) bool {
		return ctx.Value(oevCtxKey{}) == "marker"
	}), nil)

	var observedCtx int
	err := v.VarCtxObserved(ctx, 3, "oev_even,oev_ctxmark", func(ctx context.Context, ev *ValidationEvent) error {
		if ctx.Value(oevCtxKey{}) != "marker" {
			t.Errorf("observer ctx not propagated")
		}
		observedCtx++
		c.events = append(c.events, ev)
		return nil
	})
	assert.NotEqual(t, err, nil)
	assert.Equal(t, observedCtx, 1) // oev_even 失败后短路，ctxmark 不执行
	assert.Equal(t, len(c.events), 1)
	assert.Equal(t, c.events[0].Tag, "oev_even")
	assert.Equal(t, c.events[0].Pass, false)
	assert.NotEqual(t, c.events[0].FieldError, nil)

	// 通过路径两个自定义规则都执行
	c2 := new(oevCollector)
	err = v.VarCtxObserved(ctx, 4, "oev_even,oev_ctxmark", c2.observe)
	assert.Equal(t, err, nil)
	assert.Equal(t, len(c2.events), 2)
	assert.Equal(t, c2.events[0].Pass, true)
	assert.Equal(t, c2.events[1].Tag, "oev_ctxmark")
	assert.Equal(t, c2.events[1].Pass, true)
}

// ---- 空值跳过：omitempty / omitnil / omitzero 命中时不产生事件 ----

func TestObservedEmptyValueSkips(t *testing.T) {
	v := New()

	c1 := new(oevCollector)
	assert.Equal(t, v.VarCtxObserved(context.Background(), "", "omitempty,required,min=1", c1.observe), nil)
	assert.Equal(t, len(c1.events), 0)

	c2 := new(oevCollector)
	err := v.VarCtxObserved(context.Background(), "a", "omitempty,required,min=3", c2.observe)
	assert.NotEqual(t, err, nil)
	assert.Equal(t, len(c2.events), 2) // omitempty 本身无事件；required 通过、min 失败
	assert.Equal(t, c2.events[0].Tag, "required")
	assert.Equal(t, c2.events[1].Tag, "min")

	c3 := new(oevCollector)
	var np *int
	assert.Equal(t, v.VarCtxObserved(context.Background(), np, "omitnil,min=1", c3.observe), nil)
	assert.Equal(t, len(c3.events), 0)

	c4 := new(oevCollector)
	assert.Equal(t, v.VarCtxObserved(context.Background(), 0, "omitzero,min=1", c4.observe), nil)
	assert.Equal(t, len(c4.events), 0)
}

// ---- 无类型 nil / nil 接口 ----

func TestObservedUntypedNil(t *testing.T) {
	v := New()
	c := new(oevCollector)
	err := v.VarCtxObserved(context.Background(), nil, "required", c.observe)
	assert.NotEqual(t, err, nil)
	assert.Equal(t, len(c.events), 1)
	ev := c.events[0]
	assert.Equal(t, ev.Pass, false)
	assert.Equal(t, ev.Type, nil)
	if ev.FieldError == nil {
		t.Fatal("untyped nil failure must carry FieldError")
	}
	assert.Equal(t, ev.FieldError.Kind(), reflect.Invalid)
}

// ---- Valuer 解包后反射类型为底层类型 ----

type oevValuer struct{ S string }

func (w oevValuer) ValidatorValue() any { return w.S }

func TestObservedValuerUnwrappedType(t *testing.T) {
	v := New()
	c := new(oevCollector)
	err := v.VarCtxObserved(context.Background(), oevValuer{S: "a"}, "min=2", c.observe)
	assert.NotEqual(t, err, nil)
	assert.Equal(t, len(c.events), 1)
	oevAssertType(t, c.events[0].Type, reflect.TypeOf(""))
	oevAssertType(t, c.events[0].FieldError.Type(), reflect.TypeOf(""))
}

// ---- 结构级报告：ReportError 与 ReportValidationErrors ----

type oevSLStruct struct {
	Name string `validate:"min=2"`
	Mail string
}

func TestObservedStructLevelReports(t *testing.T) {
	v := New()
	v.RegisterStructValidation(func(sl StructLevel) {
		cur := sl.Current()

		sl.ReportError(cur.FieldByName("Name"), "Name", "Name", "oev_sl_rule", "sp")

		// 在结构级回调中重入同一个验证器（无观察者），再转发其错误
		verrs := sl.Validator().Var(cur.FieldByName("Mail").Interface(), "email")
		sl.ReportValidationErrors("Mail", "Mail", verrs.(ValidationErrors))
	}, oevSLStruct{})

	c := new(oevCollector)
	err := v.StructCtxObserved(context.Background(), oevSLStruct{Name: "ok", Mail: "bad"}, c.observe)
	verrs, ok := err.(ValidationErrors)
	assert.Equal(t, ok, true)
	assert.Equal(t, len(verrs), 2)
	oevAssertContiguousSeq(t, c.events)

	// 字段事件在前，结构级上报在后
	assert.Equal(t, len(c.events), 3)
	assert.Equal(t, c.events[0].Namespace, "oevSLStruct.Name")
	assert.Equal(t, c.events[0].Tag, "min")
	assert.Equal(t, c.events[0].Pass, true)

	rep1 := c.events[1]
	assert.Equal(t, rep1.Namespace, "oevSLStruct.Name")
	assert.Equal(t, rep1.Tag, "oev_sl_rule")
	assert.Equal(t, rep1.Param, "sp")
	assert.Equal(t, rep1.Pass, false)
	assert.Equal(t, rep1.FieldError, verrs[0])

	rep2 := c.events[2]
	assert.Equal(t, rep2.Namespace, "oevSLStruct.Mail")
	assert.Equal(t, rep2.Tag, "email")
	assert.Equal(t, rep2.Pass, false)
	assert.Equal(t, rep2.FieldError, verrs[1])
}

// ---- 观察函数返回错误：立即停止、原样返回、状态重置后可继续使用 ----

type oevStopStruct struct {
	A string `validate:"required,min=1"`
	B string `validate:"required,min=1"`
	C string `validate:"required,min=1"`
}

func TestObservedObserverErrorStops(t *testing.T) {
	v := New()
	sentinel := errors.New("oev stop")

	var got []*ValidationEvent
	err := v.StructCtxObserved(context.Background(), &oevStopStruct{A: "a", B: "b", C: "c"},
		func(_ context.Context, ev *ValidationEvent) error {
			got = append(got, ev)
			if ev.Seq == 2 {
				return sentinel
			}
			return nil
		})

	// 原样返回观察函数的错误，而不是 ValidationErrors
	assert.Equal(t, err, sentinel)
	if _, ok := err.(ValidationErrors); ok {
		t.Fatal("observer error must not be wrapped as ValidationErrors")
	}
	assert.Equal(t, len(got), 2) // 第二个事件送达时返回错误，立即停止
	assert.Equal(t, got[0].Seq, uint64(1))
	assert.Equal(t, got[1].Seq, uint64(2))
	assert.Equal(t, got[1].Namespace, "oevStopStruct.A")
	assert.Equal(t, got[1].Tag, "min")

	// 池化状态已重置：新的调用序号从头开始且完整执行
	c := new(oevCollector)
	err = v.StructCtxObserved(context.Background(), &oevStopStruct{A: "a", B: "b", C: "c"}, c.observe)
	assert.Equal(t, err, nil)
	assert.Equal(t, len(c.events), 6)
	oevAssertContiguousSeq(t, c.events)
}

// dive 遍历途中中止：剩余元素不再遍历
func TestObservedObserverErrorStopsDive(t *testing.T) {
	v := New()
	sentinel := errors.New("oev dive stop")

	var got []*ValidationEvent
	err := v.VarCtxObserved(context.Background(), []string{"a", "b", "c", "d"}, "dive,required",
		func(_ context.Context, ev *ValidationEvent) error {
			got = append(got, ev)
			if ev.Namespace == "[1]" {
				return sentinel
			}

			return nil
		})

	assert.Equal(t, err, sentinel)
	assert.Equal(t, len(got), 2)
	assert.Equal(t, got[0].Namespace, "[0]")
	assert.Equal(t, got[1].Namespace, "[1]")
}

// 结构级上报触发中止：字段没有规则，首个事件即结构级 ReportError
type oevSLStopStruct struct {
	A string
	B string
}

func TestObservedObserverErrorDuringStructLevel(t *testing.T) {
	v := New()
	sentinel := errors.New("oev sl stop")
	v.RegisterStructValidation(func(sl StructLevel) {
		cur := sl.Current()
		sl.ReportError(cur.FieldByName("A"), "A", "A", "oev_x", "")
		sl.ReportError(cur.FieldByName("B"), "B", "B", "oev_y", "") // 不会再送达
	}, oevSLStopStruct{})

	var n int
	var lastNS string
	err := v.StructCtxObserved(context.Background(), &oevSLStopStruct{}, func(_ context.Context, ev *ValidationEvent) error {
		n++
		lastNS = ev.Namespace
		return sentinel
	})
	assert.Equal(t, err, sentinel)
	assert.Equal(t, n, 1)
	assert.Equal(t, lastNS, "oevSLStopStruct.A")
	assert.Equal(t, err.Error(), "oev sl stop")
}

// ---- nil 观察者 / 既有入口行为完全兼容 ----

func oevProjectFE(fe FieldError) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s|%v|%v|%v",
		fe.Tag(), fe.ActualTag(), fe.Namespace(), fe.StructNamespace(),
		fe.Field(), fe.StructField(), fe.Param(), fe.Kind(), fe.Type())
}

func oevAssertSameErrors(t *testing.T, a, b error) {
	t.Helper()
	if a == nil || b == nil {
		assert.Equal(t, a == nil, b == nil)
		return
	}
	av, aok := a.(ValidationErrors)
	bv, bok := b.(ValidationErrors)
	assert.Equal(t, aok, true)
	assert.Equal(t, bok, true)
	assert.Equal(t, len(av), len(bv))
	for i := range av {
		assert.Equal(t, oevProjectFE(av[i]), oevProjectFE(bv[i]))
		assert.Equal(t, av[i].Value(), bv[i].Value())
	}
}

func TestObservedNilObserverCompatibility(t *testing.T) {
	v := New()
	v.RegisterAlias("oev_bounded", "min=2,max=5")
	ctx := context.Background()

	// 结构体：嵌套、slice dive、失败与成功混合
	bad := &oevBasicOuter{
		Name:  "a",
		Inner: oevBasicInner{Street: "ab"},
		Items: []string{"ok", ""},
	}
	oevAssertSameErrors(t, v.StructCtx(ctx, bad), v.StructCtxObserved(ctx, bad, nil))

	good := &oevBasicOuter{Name: "abc", Inner: oevBasicInner{Street: "abc"}, Items: []string{"ok"}, Ptr: new(string)}
	oevAssertSameErrors(t, v.StructCtx(ctx, good), v.StructCtxObserved(ctx, good, nil))

	// 非法输入：错误类型一致
	errA := v.StructCtx(ctx, 123)
	errB := v.StructCtxObserved(ctx, 123, nil)
	_, aok := errA.(*InvalidValidationError)
	_, bok := errB.(*InvalidValidationError)
	assert.Equal(t, aok, true)
	assert.Equal(t, bok, true)

	// Var：普通规则、别名、or、slice dive、nil 指针
	varCases := []struct {
		field interface{}
		tag   string
	}{
		{"a", "oev_bounded"},
		{"zzz", "rgb|rgba"},
		{[]string{"ok", ""}, "required,dive,required"},
		{(*string)(nil), "required"},
		{nil, "required"},
		{"abc", "min=1,max=10"},
		{0, "omitzero,min=1"},
	}
	for _, tc := range varCases {
		oevAssertSameErrors(t, v.VarCtx(ctx, tc.field, tc.tag), v.VarCtxObserved(ctx, tc.field, tc.tag, nil))
	}

	// 空标签与 "-"：两边都为 nil，且观察函数永不被调用
	c := new(oevCollector)
	assert.Equal(t, v.VarCtxObserved(ctx, "x", "", c.observe), nil)
	assert.Equal(t, v.VarCtxObserved(ctx, "x", "-", c.observe), nil)
	assert.Equal(t, len(c.events), 0)

	// 既有入口（完全不传观察能力）返回行为不变
	oevAssertSameErrors(t, v.Struct(bad), v.StructCtxObserved(ctx, bad, nil))
	assert.Equal(t, v.Var("a", "oev_bounded") != nil, true)
}

// ---- Var 入口：slice 元素路径与嵌套结构前缀 ----

func TestObservedVarPaths(t *testing.T) {
	v := New()
	ctx := context.Background()

	c1 := new(oevCollector)
	err := v.VarCtxObserved(ctx, []string{"ok", ""}, "required,dive,required,min=2", c1.observe)
	assert.NotEqual(t, err, nil)
	oevAssertContiguousSeq(t, c1.events)
	// 切片自身 required；[0] required+min=2 通过；[1] required 失败后短路
	assert.Equal(t, len(c1.events), 4)
	assert.Equal(t, c1.events[0].Namespace, "") // 切片自身无字段名
	assert.Equal(t, c1.events[0].Tag, "required")
	assert.Equal(t, oevFindEvent(t, c1.events, "[0]", "required").Pass, true)
	assert.Equal(t, oevFindEvent(t, c1.events, "[0]", "min").Pass, true)
	failOne := oevFindEvent(t, c1.events, "[1]", "required")
	assert.Equal(t, failOne.Pass, false)
	assert.NotEqual(t, failOne.FieldError, nil)
	for _, ev := range c1.events {
		if ev.Namespace == "[1]" && ev.Tag == "min" {
			t.Fatal("min should be short-circuited")
		}
	}

	// Var 直接验证嵌套结构：带类型名前缀
	c2 := new(oevCollector)
	err = v.VarCtxObserved(ctx, oevBasicInner{Street: "x"}, "required", c2.observe)
	assert.NotEqual(t, err, nil)
	streetFail := oevFindEvent(t, c2.events, "oevBasicInner.Street", "min")
	assert.Equal(t, streetFail.Pass, false)
}

// ---- 事件快照稳定性：内部 ns/misc 缓冲复用不得改变已发出事件 ----

type oevSnapChild struct {
	Street string `validate:"required,min=4"`
}

type oevSnapOuter struct {
	Items []oevSnapChild `validate:"dive"`
}

func TestObservedEventSnapshotStability(t *testing.T) {
	v := New()
	c := new(oevCollector)

	s := oevSnapOuter{Items: []oevSnapChild{{Street: "abc"}, {Street: "abcd"}, {Street: "ab"}}}
	err := v.StructCtxObserved(context.Background(), s, c.observe)
	assert.NotEqual(t, err, nil)

	snapshot := make([]string, len(c.events))
	for i, ev := range c.events {
		snapshot[i] = fmt.Sprintf("%d:%s:%s:%s:%t", ev.Seq, ev.Namespace, ev.StructNamespace, ev.Tag, ev.Pass)
	}

	// 大量后续调用复用池化的 ns / misc / str1 / str2 缓冲
	for i := 0; i < 20; i++ {
		_ = v.StructCtx(context.Background(), &oevBasicOuter{
			Name:  "zzzzzz",
			Inner: oevBasicInner{Street: "zzzzzz"},
			Items: []string{"zzzzzz", "q"},
			Ptr:   new(string),
		})
		_ = v.VarCtx(context.Background(), []string{"zzzzzz"}, "dive,min=2")
	}

	// 已保留的事件路径与内容不被后续调用改写
	for i, ev := range c.events {
		got := fmt.Sprintf("%d:%s:%s:%s:%t", ev.Seq, ev.Namespace, ev.StructNamespace, ev.Tag, ev.Pass)
		assert.Equal(t, got, snapshot[i])
		if ev.FieldError != nil {
			assert.NotEqual(t, ev.FieldError.Namespace(), "")
		}
	}

	//快速抽查关键路径唯一且正确
	oevFindEvent(t, c.events, "oevSnapOuter.Items[0].Street", "min")
	oevFindEvent(t, c.events, "oevSnapOuter.Items[2].Street", "min")
}

// ---- 并发：观察状态按调用隔离，无数据竞争 ----

type oevConcStruct struct {
	Label string   `validate:"required,min=2,max=20"`
	Num   int      `validate:"oev_even"`
	Items []string `validate:"dive,required,min=1"`
}

func TestObservedConcurrentIsolation(t *testing.T) {
	v := New()
	assert.Equal(t, v.RegisterValidation("oev_even", func(fl FieldLevel) bool {
		return fl.Field().Int()%2 == 0
	}), nil)
	v.RegisterAlias("oev_bounded", "min=2,max=5")

	const goroutines = 32
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()

			for i := 0; i < iterations; i++ {
				local := new(oevCollector)

				s := &oevConcStruct{
					Label: fmt.Sprintf("g%d-label", g),
					Num:   int(i % 2), // 奇偶交替制造通过/失败
					Items: []string{"a", "bb"},
				}

				var err error
				if i%2 == 0 {
					err = v.StructCtxObserved(context.Background(), s, local.observe)
					// 并发交错使用普通入口，确保不携带任何观察状态
					_ = v.VarCtx(context.Background(), "abc", "oev_bounded")
				} else {
					err = v.VarCtxObserved(context.Background(), i, "oev_even", local.observe)
				}

				// 每次调用的序号都必须从 1 开始连续
				oevAssertContiguousSeq(t, local.events)

				switch e := err.(type) {
				case nil:
				case ValidationErrors:
					_ = e
				default:
					t.Errorf("unexpected error type: %T", err)
				}

				if i%2 == 0 {
					// 结构体路径固定 8 个事件：Label 三条、Num 一条、两个元素各 required+min
					assert.Equal(t, len(local.events), 8)
					assert.Equal(t, local.events[0].Namespace, "oevConcStruct.Label")
					assert.Equal(t, local.events[len(local.events)-1].Namespace, "oevConcStruct.Items[1]")
				} else {
					assert.Equal(t, len(local.events), 1)
				}
			}
		}(g)
	}

	wg.Wait()
}

// ---- 重入：观察回调内重入同一个验证器，内外状态互不污染 ----

func TestObservedReentrancy(t *testing.T) {
	v := New()
	v.RegisterAlias("oev_bounded", "min=2,max=5")

	var nested []*ValidationEvent
	nestedObs := func(_ context.Context, ev *ValidationEvent) error {
		nested = append(nested, ev)
		return nil
	}

	var outer []*ValidationEvent
	var reentries int

	err := v.VarCtxObserved(context.Background(), "abc", "min=1,max=10",
		func(ctx context.Context, ev *ValidationEvent) error {
			outer = append(outer, ev)

			if ev.Seq == 1 {
				reentries++
				// 被观察的嵌套调用：独立序号、独立收集器，min=2 对 "a" 失败
				innerErr := v.VarCtxObserved(ctx, "a", "min=2", nestedObs)
				assert.NotEqual(t, innerErr, nil)

				// 未观察的嵌套调用
				assert.Equal(t, v.VarCtx(ctx, 5, "min=1"), nil)

				// 嵌套结构体调用，验证池化对象可嵌套获取
				assert.Equal(t, v.StructCtx(ctx, &oevStopStruct{A: "a", B: "b", C: "c"}), nil)
			}
			return nil
		})

	assert.Equal(t, err, nil)
	assert.Equal(t, reentries, 1)

	// 外层事件不受嵌套调用影响：序号连续、只有两条
	assert.Equal(t, len(outer), 2)
	oevAssertContiguousSeq(t, outer)
	assert.Equal(t, outer[0].ActualTag, "min")
	assert.Equal(t, outer[1].ActualTag, "max")

	// 嵌套调用拥有自己从 1 开始的序号
	assert.Equal(t, len(nested), 1)
	assert.Equal(t, nested[0].Seq, uint64(1))
	assert.Equal(t, nested[0].ActualTag, "min")
	assert.Equal(t, nested[0].Pass, false)

	// 嵌套调用结束后，外层验证器仍可正常发起全新观察调用，序号重新开始
	c2 := new(oevCollector)
	assert.Equal(t, v.VarCtxObserved(context.Background(), "ab", "oev_bounded", c2.observe), nil)
	assert.Equal(t, len(c2.events), 2)
	oevAssertContiguousSeq(t, c2.events)
}
