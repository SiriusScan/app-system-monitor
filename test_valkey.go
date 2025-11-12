package main
import (
	"context"
	"fmt"
	"github.com/SiriusScan/go-api/sirius/store"
)
func main() {
	fmt.Println("Attempting to create Valkey store...")
	valkeyStore, err := store.NewValkeyStore()
	if err != nil {
		fmt.Printf("ERROR creating Valkey store: %v\n", err)
		return
	}
	fmt.Println("SUCCESS: Valkey store created")
	ctx := context.Background()
	err = valkeyStore.SetValue(ctx, "test:key", "test_value")
	if err != nil {
		fmt.Printf("ERROR setting value: %v\n", err)
		return
	}
	fmt.Println("SUCCESS: Value set in Valkey")
}
