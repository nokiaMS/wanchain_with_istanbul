package main

import (
	"fmt"
	"sync"
)

//sync.Once: once对象只会执行一次,不管Do()内的函数是相同还是不同.
func case1()  {
	var once sync.Once

	once.Do(func2)
	once.Do(func1)

	for i := 0; i < 10;i++  {
		once.Do(func1)
	}
	for i := 0; i < 10;i++  {
		once.Do(func2)
	}
}

func func1()  {
	fmt.Println("func1.")
}

func func2()  {
	fmt.Println("func2.")
}

//接口泛化与细化.	inst.(Type)表示把inst实例强制转换为Type类型,这样就可以调用type类型特有的方法了.
type testInterface interface { //定义了一个接口.
	show()
}

//接口实现1.
type testInstance1 struct {
}

func (inst testInstance1) show()  {
	fmt.Println("This is a instance 1 for interface.")
}

func (inst testInstance1) run1() {
	fmt.Println("The type of this instance is testInstance1.")
}

//接口实现2
type testInstance2 struct {
}

func (inst testInstance2) show()  {
	fmt.Println("This is a instance 2 for interface.")
}

func (inst testInstance2) run2()  {
	fmt.Println("The type of this instance is testInstance2.")
}

func case2( i testInterface)  {
	i.show()
	//t1 := i.(testInstance2)		//接口具型(细化),把泛化的接口强制转化为具体的类型,这样就可以调用具体类型的特有函数了.
	//t1.run2()	//调用具体类型的特有函数.

	//如果传递的是testInstance2的实例,但是此处要转换成testInstance2的实例,那么会报错.
	t1 := i.(testInstance1)		//接口具型(细化),把泛化的接口强制转化为具体的类型,这样就可以调用具体类型的特有函数了.
	t1.run1()	//调用具体类型的特有函数.

	switch i.(type) {
	case testInstance1:
		fmt.Println("aaaa inst1")
	case testInstance2:
		fmt.Println("aaaa inst2")
	default:
		fmt.Println("neither inst1 nor inst2.")
	}

}

func main()  {
	fmt.Println("This is a test program.")
	case1()
	case2(testInstance1{})
}
