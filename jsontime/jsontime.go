package utils

import (
	"database/sql/driver"
	"fmt"
	"time"
)

// timeFormat 定义了时间格式，用于时间的字符串表示和解析
const timeFormat = "2006-01-02 15:04:05"

// timezone 定义了时间的时区，用于将时间转换到特定的时区
const timeZone = "Asia/Shanghai"

func location() *time.Location {
	loc, err := time.LoadLocation(timeZone)
	if err != nil {
		return time.Local
	}
	return loc
}

// Time 是 time.Time 的别名，用于添加自定义的序列化和反序列化行为
type Time time.Time

// MarshalJSON 实现了 Time 类型的自定义 JSON 序列化
// 返回值:
// - []byte: 序列化后的 JSON 字符串
// - error: 错误信息
func (t Time) MarshalJSON() ([]byte, error) {
	ti := time.Time(t)
	if ti.IsZero() {
		return []byte("null"), nil
	}
	b := make([]byte, 0, len(timeFormat)+2)
	b = append(b, '"')
	b = ti.AppendFormat(b, timeFormat)
	b = append(b, '"')
	return b, nil
}

// UnmarshalJSON 实现了 Time 类型的自定义 JSON 反序列化
// 参数:
// - data: 需要反序列化的 JSON 数据
// 返回值:
// - error: 错误信息
func (t *Time) UnmarshalJSON(data []byte) (err error) {
	s := string(data)
	if s == "null" || s == "NULL" {
		*t = Time(time.Time{})
		return nil
	}
	if s == `""` {
		*t = Time(time.Time{})
		return nil
	}
	now, err := time.ParseInLocation(`"`+timeFormat+`"`, s, location())
	if err != nil {
		return err
	}
	*t = Time(now)
	return nil
}

// String 实现了 Time 类型的自定义字符串表示
// 返回值:
// - string: 时间的字符串表示
func (t Time) String() string {
	// 格式化时间并返回字符串表示
	return time.Time(t).Format(timeFormat)
}

// Local 返回 Time 类型的时间在指定时区的表示
// 返回值:
// - time.Time: 转换后的本地时间
func (t Time) Local() time.Time {
	// 将时间转换到指定的时区并返回
	return time.Time(t).In(location())
}

// Value 实现了 Time 类型的数据库驱动值表示
// 返回值:
// - driver.Value: 数据库驱动值
// - error: 错误信息
func (t Time) Value() (driver.Value, error) {
	ti := time.Time(t)
	if ti.IsZero() {
		return nil, nil //nolint:nilnil
	}
	return ti, nil
}

// Scan 实现了 Time 类型的数据库驱动值反序列化
// 参数:
// - v: 数据库驱动值
// 返回值:
// - error: 错误信息
func (t *Time) Scan(v interface{}) error {
	if v == nil {
		*t = Time(time.Time{})
		return nil
	}
	value, ok := v.(time.Time)
	if ok {
		*t = Time(value.In(location()))
		return nil
	}
	return fmt.Errorf("can not convert %v to timestamp", v)
}

// 补充一些实用函数，以快速将字符串转换为时间类型，获取time.Time类型的当前时间
func (t Time) Time() time.Time {
	return time.Time(t)
}

// Now 返回当前时间的 Time 类型表示
// 返回值:
// - Time: 当前时间的 Time 类型表示
func Now() Time {
	return Time(time.Now().In(location()))
}

// FromString 从字符串创建 Time 类型
// 参数:
// - s: 时间字符串
// 返回值:
// - Time: 解析后的 Time 类型
func FromString(s string) (Time, error) {
	// 解析字符串中的时间
	t, err := time.ParseInLocation(timeFormat, s, location())
	// 检查解析是否出错
	if err != nil {
		// 返回解析错误
		return Time{}, err
	}
	// 返回解析后的 Time 类型
	return Time(t), nil
}

func FromTime(t time.Time) Time {
	return Time(t.In(location()))
}

func FromUnix(sec int64) Time {
	return FromTime(time.Unix(sec, 0))
}

func FromUnixMilli(ms int64) Time {
	return FromTime(time.Unix(0, ms*int64(time.Millisecond)))
}

func FromUnixNano(ns int64) Time {
	return FromTime(time.Unix(0, ns))
}
