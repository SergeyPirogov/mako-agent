import { ClaudeHeadlessExecutor, type ClaudeHeadlessConfig } from "./claude-headless.js";
import { CommandExecutor, type CommandExecutorConfig } from "./command.js";
import { OrcaExecutor, type OrcaExecutorConfig } from "./orca.js";
import { TerminalExecutor, type TerminalExecutorConfig } from "./terminal.js";
import type { Executor } from "./types.js";

export type ExecutorConfig = ClaudeHeadlessConfig | CommandExecutorConfig | OrcaExecutorConfig | TerminalExecutorConfig;
export type { Executor, ExecContext, ExecOutcome } from "./types.js";
export { killAllAgents } from "./processes.js";

export function createExecutor(name: string, cfg: ExecutorConfig): Executor {
  switch (cfg.type) {
    case "claude-headless":
      return new ClaudeHeadlessExecutor(name, cfg);
    case "command":
      return new CommandExecutor(name, cfg);
    case "orca":
      return new OrcaExecutor(name, cfg);
    case "terminal":
      return new TerminalExecutor(name, cfg);
    default:
      throw new Error(`Executor ${name}: unknown type ${(cfg as any).type}. Use terminal, claude-headless, orca or command`);
  }
}
