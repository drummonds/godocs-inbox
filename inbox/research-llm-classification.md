# LLM-based Document Classification

## Idea
Use a local LLM (e.g. Ollama with llama3) to auto-suggest tags for inbox items.

## Approach
1. Feed document content to local model
2. Model returns suggested tag + confidence
3. User confirms or overrides with keystroke

## Open Questions
- Which model gives best accuracy for short docs?
- Latency budget: needs to feel instant (<500ms)
- Fine-tuning vs few-shot prompting?
