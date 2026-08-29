package skillreview

// ModuleStatus 表示评审系统模块当前的实现状态。
type ModuleStatus string

const (
	ModuleActive  ModuleStatus = "active"
	ModulePlanned ModuleStatus = "planned"
)

// ReviewModule 是左侧评审流程导航和后续模块路由的统一描述。
type ReviewModule struct {
	ID          string       `json:"id"`
	Order       int          `json:"order"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Status      ModuleStatus `json:"status"`
}

// ReviewModules 返回 Skill 评审系统的标准流程。
// 当前只启用路径发现，其余模块先保留稳定的模块边界，后续分别实现。
func ReviewModules() []ReviewModule {
	return []ReviewModule{
		{ID: "path-discovery", Order: 1, Name: "路径发现", Description: "生成并校验 Skill Path", Status: ModuleActive},
		{ID: "test-dataset", Order: 2, Name: "测试数据集", Description: "为用户输入准备评审合同", Status: ModulePlanned},
		{ID: "unit-test", Order: 3, Name: "单元测试", Description: "执行链路与覆盖率", Status: ModulePlanned},
		{ID: "targeted-fix", Order: 4, Name: "定向修复", Description: "关联失败节点与 Skill 修改", Status: ModulePlanned},
		{ID: "full-regression", Order: 5, Name: "全量回归", Description: "验证修改没有回归", Status: ModulePlanned},
	}
}
