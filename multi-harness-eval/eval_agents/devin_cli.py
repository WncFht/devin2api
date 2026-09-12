"""Devin CLI as a Harbor installed agent.

swe-2-max 的原生 harness:不经过 devin-2api/ccload,容器内 CLI 直连
api.devin.ai / server.codeium.com,认证靠预置 credentials.toml。

用法:
  harbor run -d <dataset> --agent multi-harness-eval.agents.devin_cli:DevinCli \
    -m devin/swe-2-max --agent-env DEVIN_SESSION_TOKEN=<token>
"""

import json
import shlex

from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.agents.options import InstalledAgentOptions
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext
from harbor.models.trial.paths import EnvironmentPaths

ATIF_EXPORT = EnvironmentPaths.agent_dir / "devin-cli.atif.json"

# credentials.toml 的字段取自本机 ~/.local/share/devin/credentials.toml 的真实结构
CREDENTIALS_SNIPPET = (
    'windsurf_api_key = "$DEVIN_SESSION_TOKEN"\n'
    'api_server_url = "https://server.codeium.com"\n'
    'devin_webapp_host = "app.devin.ai"\n'
    'devin_api_url = "https://api.devin.ai"\n'
)


class DevinCli(BaseInstalledAgent):
    """Devin CLI harness (`devin -p` headless 模式 + `--export` ATIF 轨迹)。"""

    options_model = InstalledAgentOptions

    @staticmethod
    def name() -> str:
        """Agent 名,job yaml / --agent 里用。"""
        return "devin-cli"

    def get_version_command(self) -> str | None:
        """容器内 devin 版本查询命令。"""
        return 'export PATH="$HOME/.local/bin:$PATH"; devin --version'

    def parse_version(self, stdout: str) -> str:
        """`devin 3000.10.21 (611c1cba)` → `3000.10.21`。"""
        return stdout.strip().split()[1] if len(stdout.split()) > 1 else stdout.strip()

    async def install(self, environment: BaseEnvironment) -> None:
        """装 CLI 二进制并写入 credentials.toml(token 来自 agents[].env 的 DEVIN_SESSION_TOKEN)。"""
        await self.ensure_system_dependencies(environment, ("curl", "bash", "ca-certificates"))
        await self.exec_as_agent(
            environment,
            command="curl -fsSL https://cli.devin.ai/install.sh | bash",
        )
        await self.exec_as_agent(
            environment,
            command=(
                'mkdir -p "$HOME/.local/share/devin" && '
                f'printf \'{CREDENTIALS_SNIPPET}\' '
                '> "$HOME/.local/share/devin/credentials.toml" && '
                'chmod 600 "$HOME/.local/share/devin/credentials.toml"'
            ),
        )

    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        """headless 跑一条任务指令,轨迹经 --export 落 ATIF。"""
        model = (self.model_name or "swe-2-max").rsplit("/", 1)[-1]
        await self.exec_as_agent(
            environment,
            command=(
                'export PATH="$HOME/.local/bin:$PATH"; '
                f"devin -p --model {shlex.quote(model)} "
                "--permission-mode dangerous "
                "--respect-workspace-trust false "
                f"--export {shlex.quote(str(ATIF_EXPORT))} "
                f"-- {shlex.quote(instruction)} "
                "2>&1 </dev/null | tee /logs/agent/devin-cli.txt"
            ),
        )

    def populate_context_post_run(self, context: AgentContext) -> None:
        """读 ATIF 导出,累计 token/成本到 context(字段缺失则跳过)。"""
        export = self.logs_dir / ATIF_EXPORT.name
        if not export.exists():
            return
        steps = json.loads(export.read_text()).get("steps", [])
        for step in steps:
            metrics = step.get("metrics") or {}
            for key, attr in (
                ("prompt_tokens", "n_input_tokens"),
                ("completion_tokens", "n_output_tokens"),
                ("cached_tokens", "n_cache_tokens"),
                ("cost_usd", "cost_usd"),
            ):
                if metrics.get(key):
                    current = getattr(context, attr) or 0
                    setattr(context, attr, current + metrics[key])
