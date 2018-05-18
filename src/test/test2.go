package main

import "fmt"

/*
	go的reflect库有两个重要的类型:
		reflect.Type  //对应对象的类型.
		reflect.Value  //对应对象的值.
	还有两个重要的函数:
		reflect.TypeOf(i interface()) Type  	//返回对象的类型 reflect.Type
		reflect.ValueOf(i interface()) Value 	//返回对象的值 reflect.Value
*/

func main()  {
	fmt.Println("This is a test program.")
}
