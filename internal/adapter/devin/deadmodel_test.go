// 本文件验证死模型登记：上游 permission_denied 与目录缺席双实证后
// 本地拒绝，目录内模型与非权限拒绝不连坐，标记到期/目录收录自动放行。
package devin

import (
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/llm"
)

func TestDeadModelMarkAndRefusal(t *testing.T) {
	denied := &llm.Failure{Code: "permission_denied", Message: "an internal error occurred"}
	subject := &Adapter{models: []adapter.ModelInfo{{ID: "alive"}}}

	// 目录缺席 + permission_denied → 标记并本地拒绝（not_found）。
	subject.noteModelDenied("ghost", denied)
	err := subject.checkDeadModel("ghost")
	if err == nil {
		t.Fatal("denied absent model must be refused locally")
	}
	if failure := llm.Classify(err); failure.Code != "not_found" {
		t.Fatalf("dead-model refusal should surface as not_found, got %q", failure.Code)
	}

	// 目录内模型的同 code 拒绝是内容策略/授权别案——不登记不连坐。
	subject.noteModelDenied("alive", denied)
	if err := subject.checkDeadModel("alive"); err != nil {
		t.Fatalf("in-catalog model must not be marked dead: %v", err)
	}

	// 非 permission_denied 不构成死模型证据。
	subject.noteModelDenied("ghost2", &llm.Failure{Code: "invalid_argument", Message: "bad request"})
	if err := subject.checkDeadModel("ghost2"); err != nil {
		t.Fatalf("non-permission_denied must not mark dead: %v", err)
	}

	// 目录从未加载时缺席无从判定——不登记。
	empty := &Adapter{}
	empty.noteModelDenied("ghost", denied)
	if err := empty.checkDeadModel("ghost"); err != nil {
		t.Fatalf("unloaded catalog must not mark dead: %v", err)
	}

	// 目录重新收录 → 标记作废放行。
	subject.models = append(subject.models, adapter.ModelInfo{ID: "ghost"})
	if err := subject.checkDeadModel("ghost"); err != nil {
		t.Fatalf("catalog re-inclusion must lift the dead mark: %v", err)
	}
}

func TestDeadModelExpiry(t *testing.T) {
	denied := &llm.Failure{Code: "permission_denied", Message: "an internal error occurred"}
	subject := &Adapter{models: []adapter.ModelInfo{{ID: "alive"}}}
	subject.noteModelDenied("ghost", denied)
	subject.deadModels["ghost"] = time.Now().Add(-time.Second)
	if err := subject.checkDeadModel("ghost"); err != nil {
		t.Fatalf("expired mark must not refuse: %v", err)
	}
}
