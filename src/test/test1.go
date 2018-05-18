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

func main()  {
	fmt.Println("This is a test program.")
	case1()
}
