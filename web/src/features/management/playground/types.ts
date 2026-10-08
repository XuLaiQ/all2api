export type ComposerMode = "chat" | "image" | "ppt" | "video" | "writing" | "quiz" | "transcribe" | "search" | "psd";

export type PlaygroundGenerationSettings = {
  imageRatio: string;
  videoRatio: string;
  videoDuration: number;
};

export type PlaygroundSidebarTab = "conversations" | "requests";

export type ChatMessage = {
  id: string;
  role: "user" | "assistant" | "error";
  content: string;
  model?: string;
  raw?: unknown;
};

export const chatSuggestionOptions = [
  { label: "写一段代码", prompt: "用 Python 实现一个 LRU 缓存，并解释关键设计。" },
  { label: "解释一个概念", prompt: "用生活中的例子解释什么是 CAP 定理。" },
  { label: "整理成表格", prompt: "把常见的 API 鉴权方式整理成一张对比表。" },
] as const;
