import { Component, type ErrorInfo, type ReactNode } from "react";
import { RefreshCw } from "lucide-react";

interface Props {
  children: ReactNode;
  resetKey?: string;
}

interface State {
  hasError: boolean;
}

export class ChunkErrorBoundary extends Component<Props, State> {
  state: State = { hasError: false };

  static getDerivedStateFromError(): State {
    return { hasError: true };
  }

  componentDidUpdate(prevProps: Props) {
    if (this.state.hasError && prevProps.resetKey !== this.props.resetKey) {
      this.setState({ hasError: false });
    }
  }

  componentDidCatch(_error: Error, _info: ErrorInfo) {
    // Handled via recovery UI when stale/unreachable async chunk fails to load.
  }

  render() {
    if (!this.state.hasError) {
      return this.props.children;
    }
    return (
      <div className="w-full h-full min-h-[360px] rounded-xl border border-hairline surface-rail flex flex-col items-center justify-center gap-3 p-6 text-center">
        <p className="text-xs text-secondary">控制台已更新或加载失败，点击刷新</p>
        <button
          type="button"
          onClick={() => window.location.reload()}
          className="inline-flex items-center gap-1.5 px-3.5 py-1.5 rounded-lg text-xs font-medium btn-accent cursor-pointer"
        >
          <RefreshCw className="w-3.5 h-3.5" />
          <span>立即刷新</span>
        </button>
      </div>
    );
  }
}
