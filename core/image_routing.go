package core

// ImageInputMode 图像输入模式
type ImageInputMode string

const (
	ImageModeAuto   ImageInputMode = "auto"   // 自动决定
	ImageModeNative ImageInputMode = "native" // 原生图像附件
	ImageModeText   ImageInputMode = "text"   // 文本描述（vision_analyze）
)

// ImageRoute 图像路由决策结果
type ImageRoute struct {
	Provider string // 目标提供商
	Model    string // 目标模型
	Mode     ImageInputMode
}

// RouteImage 根据图像内容和模型能力决定路由策略
// imageData: 图像原始字节
// mimeType: 图像的 MIME 类型（如 "image/png"）
func RouteImage(imageData []byte, mimeType string) (*ImageRoute, error) {
	// 默认路由：如果模型支持视觉，使用 native 模式
	if len(imageData) == 0 {
		return &ImageRoute{Mode: ImageModeText}, nil
	}

	// 简单实现：返回 native 模式，由上层根据模型能力覆写
	route := &ImageRoute{
		Mode: ImageModeNative,
	}

	return route, nil
}

// DecideImageInputMode 根据模型能力和配置决定图像输入模式
// supportsVision: 模型是否支持原生视觉输入
// hasAuxVisionOverride: 用户是否显式配置了辅助视觉模型
func DecideImageInputMode(supportsVision bool, hasAuxVisionOverride bool) ImageInputMode {
	// 如果用户显式配置了辅助视觉模型，使用 text 模式
	if hasAuxVisionOverride {
		return ImageModeText
	}
	// 如果模型支持视觉，使用 native 模式
	if supportsVision {
		return ImageModeNative
	}
	// 兜底使用 text 模式
	return ImageModeText
}
