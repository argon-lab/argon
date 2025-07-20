"""
Jupyter Magic Commands for Argon

This module provides IPython magic commands for seamless Argon integration
in Jupyter notebooks.
"""

from IPython.core.magic import Magics, line_magic, cell_magic, magics_class
from IPython.core.magic_arguments import argument, magic_arguments, parse_argline
from IPython.display import display, HTML, Markdown
import json
import time
from typing import Any, Dict

from .jupyter import init_argon_notebook, get_argon_integration

@magics_class
class ArgonMagics(Magics):
    """
    Argon magic commands for Jupyter notebooks
    """
    
    @line_magic
    @magic_arguments()
    @argument('project_name', help='Name of the Argon project')
    def argon_init(self, line):
        """
        Initialize Argon for the current notebook
        
        Usage: %argon_init my-project
        """
        args = parse_argline(self.argon_init, line)
        
        try:
            integration = init_argon_notebook(args.project_name)
            display(HTML(f"""
            <div style="padding: 10px; background: #e8f5e8; border-left: 4px solid #4caf50; margin: 10px 0;">
                <strong>✅ Argon Initialized</strong><br>
                Project: <code>{args.project_name}</code><br>
                Use <code>%argon_branch branch-name</code> to set your working branch
            </div>
            """))
            return integration
        except Exception as e:
            display(HTML(f"""
            <div style="padding: 10px; background: #ffeaa7; border-left: 4px solid #fdcb6e; margin: 10px 0;">
                <strong>⚠️ Argon Initialization Failed</strong><br>
                Error: {str(e)}
            </div>
            """))
            
    @line_magic
    @magic_arguments()
    @argument('branch_name', help='Name of the branch to work with')
    def argon_branch(self, line):
        """
        Set the current Argon branch
        
        Usage: %argon_branch experiment-1
        """
        args = parse_argline(self.argon_branch, line)
        integration = get_argon_integration()
        
        if not integration:
            display(HTML("""
            <div style="padding: 10px; background: #fab1a0; border-left: 4px solid #e17055; margin: 10px 0;">
                <strong>❌ Error</strong><br>
                Argon not initialized. Use <code>%argon_init project-name</code> first.
            </div>
            """))
            return
            
        try:
            branch = integration.set_branch(args.branch_name)
            display(HTML(f"""
            <div style="padding: 10px; background: #e8f5e8; border-left: 4px solid #4caf50; margin: 10px 0;">
                <strong>🌿 Branch Set</strong><br>
                Current branch: <code>{branch.name}</code><br>
                Commit: <code>{branch.commit_hash[:8]}</code>
            </div>
            """))
            return branch
        except Exception as e:
            display(HTML(f"""
            <div style="padding: 10px; background: #ffeaa7; border-left: 4px solid #fdcb6e; margin: 10px 0;">
                <strong>⚠️ Branch Error</strong><br>
                Error: {str(e)}
            </div>
            """))
            
    @line_magic
    @magic_arguments()
    @argument('params', nargs='*', help='Parameters in key=value format')
    def argon_params(self, line):
        """
        Log experiment parameters
        
        Usage: %argon_params learning_rate=0.01 batch_size=32 epochs=100
        """
        args = parse_argline(self.argon_params, line)
        integration = get_argon_integration()
        
        if not integration:
            display(HTML("""
            <div style="padding: 10px; background: #fab1a0; border-left: 4px solid #e17055; margin: 10px 0;">
                <strong>❌ Error</strong><br>
                Argon not initialized. Use <code>%argon_init project-name</code> first.
            </div>
            """))
            return
            
        params = {}
        for param in args.params:
            if '=' in param:
                key, value = param.split('=', 1)
                # Try to convert to appropriate type
                try:
                    if '.' in value:
                        params[key] = float(value)
                    else:
                        params[key] = int(value)
                except ValueError:
                    params[key] = value
                    
        integration.log_experiment_params(params)
        
        # Display parameters table
        params_html = "<table style='border-collapse: collapse; width: 100%;'>"
        params_html += "<tr style='background: #f1f1f1;'><th style='padding: 8px; border: 1px solid #ddd;'>Parameter</th><th style='padding: 8px; border: 1px solid #ddd;'>Value</th></tr>"
        
        for key, value in params.items():
            params_html += f"<tr><td style='padding: 8px; border: 1px solid #ddd;'>{key}</td><td style='padding: 8px; border: 1px solid #ddd;'>{value}</td></tr>"
            
        params_html += "</table>"
        
        display(HTML(f"""
        <div style="padding: 10px; background: #e8f4f8; border-left: 4px solid #3498db; margin: 10px 0;">
            <strong>📊 Parameters Logged</strong><br>
            {params_html}
        </div>
        """))
        
    @line_magic
    @magic_arguments()
    @argument('metrics', nargs='*', help='Metrics in key=value format')
    def argon_metrics(self, line):
        """
        Log experiment metrics
        
        Usage: %argon_metrics accuracy=0.95 loss=0.05 f1_score=0.92
        """
        args = parse_argline(self.argon_metrics, line)
        integration = get_argon_integration()
        
        if not integration:
            display(HTML("""
            <div style="padding: 10px; background: #fab1a0; border-left: 4px solid #e17055; margin: 10px 0;">
                <strong>❌ Error</strong><br>
                Argon not initialized. Use <code>%argon_init project-name</code> first.
            </div>
            """))
            return
            
        metrics = {}
        for metric in args.metrics:
            if '=' in metric:
                key, value = metric.split('=', 1)
                try:
                    metrics[key] = float(value)
                except ValueError:
                    metrics[key] = value
                    
        integration.log_experiment_metrics(metrics)
        
        # Display metrics table
        metrics_html = "<table style='border-collapse: collapse; width: 100%;'>"
        metrics_html += "<tr style='background: #f1f1f1;'><th style='padding: 8px; border: 1px solid #ddd;'>Metric</th><th style='padding: 8px; border: 1px solid #ddd;'>Value</th></tr>"
        
        for key, value in metrics.items():
            metrics_html += f"<tr><td style='padding: 8px; border: 1px solid #ddd;'>{key}</td><td style='padding: 8px; border: 1px solid #ddd;'>{value}</td></tr>"
            
        metrics_html += "</table>"
        
        display(HTML(f"""
        <div style="padding: 10px; background: #e8f4f8; border-left: 4px solid #3498db; margin: 10px 0;">
            <strong>📈 Metrics Logged</strong><br>
            {metrics_html}
        </div>
        """))
        
    @line_magic
    @magic_arguments()
    @argument('name', help='Name for the checkpoint')
    @argument('--description', '-d', help='Description for the checkpoint')
    def argon_checkpoint(self, line):
        """
        Create a checkpoint of the current notebook state
        
        Usage: %argon_checkpoint milestone-1 --description "Completed data preprocessing"
        """
        args = parse_argline(self.argon_checkpoint, line)
        integration = get_argon_integration()
        
        if not integration:
            display(HTML("""
            <div style="padding: 10px; background: #fab1a0; border-left: 4px solid #e17055; margin: 10px 0;">
                <strong>❌ Error</strong><br>
                Argon not initialized. Use <code>%argon_init project-name</code> first.
            </div>
            """))
            return
            
        description = args.description or ""
        integration.create_checkpoint(args.name, description)
        
        display(HTML(f"""
        <div style="padding: 10px; background: #e8f5e8; border-left: 4px solid #4caf50; margin: 10px 0;">
            <strong>💾 Checkpoint Created</strong><br>
            Name: <code>{args.name}</code><br>
            Description: {description}
        </div>
        """))
        
    @line_magic
    def argon_status(self, line):
        """
        Show current Argon status
        
        Usage: %argon_status
        """
        integration = get_argon_integration()
        
        if not integration:
            display(HTML("""
            <div style="padding: 10px; background: #fab1a0; border-left: 4px solid #e17055; margin: 10px 0;">
                <strong>❌ Argon Not Initialized</strong><br>
                Use <code>%argon_init project-name</code> to get started.
            </div>
            """))
            return
            
        if not integration.current_branch:
            display(HTML("""
            <div style="padding: 10px; background: #ffeaa7; border-left: 4px solid #fdcb6e; margin: 10px 0;">
                <strong>⚠️ No Active Branch</strong><br>
                Use <code>%argon_branch branch-name</code> to set a branch.
            </div>
            """))
            return
            
        branch = integration.current_branch
        sessions = branch.metadata.get("jupyter_sessions", [])
        datasets = branch.metadata.get("datasets", [])
        models = branch.metadata.get("models", [])
        checkpoints = branch.metadata.get("checkpoints", [])
        
        display(HTML(f"""
        <div style="padding: 15px; background: #f8f9fa; border-left: 4px solid #6c757d; margin: 10px 0;">
            <strong>📊 Argon Status</strong><br><br>
            <strong>Project:</strong> {integration.project.name}<br>
            <strong>Branch:</strong> {branch.name}<br>
            <strong>Commit:</strong> {branch.commit_hash[:8]}<br>
            <strong>Sessions:</strong> {len(sessions)}<br>
            <strong>Datasets:</strong> {len(datasets)}<br>
            <strong>Models:</strong> {len(models)}<br>
            <strong>Checkpoints:</strong> {len(checkpoints)}<br>
        </div>
        """))
        
    @cell_magic
    def argon_track(self, line, cell):
        """
        Track execution of a cell with Argon
        
        Usage: 
        %%argon_track
        # Your code here
        """
        integration = get_argon_integration()
        
        if not integration:
            display(HTML("""
            <div style="padding: 10px; background: #fab1a0; border-left: 4px solid #e17055; margin: 10px 0;">
                <strong>❌ Error</strong><br>
                Argon not initialized. Use <code>%argon_init project-name</code> first.
            </div>
            """))
            return
            
        # Execute the cell and track execution
        start_time = time.time()
        
        try:
            # Execute the cell code
            result = self.shell.run_cell(cell)
            execution_time = time.time() - start_time
            
            # Track the execution
            cell_id = f"cell_{int(time.time())}"
            integration.track_cell_execution(
                cell_id=cell_id,
                code=cell,
                output=result.result,
                execution_time=execution_time
            )
            
            display(HTML(f"""
            <div style="padding: 10px; background: #e8f5e8; border-left: 4px solid #4caf50; margin: 10px 0;">
                <strong>✅ Cell Tracked</strong><br>
                Execution time: {execution_time:.2f}s<br>
                Cell ID: <code>{cell_id}</code>
            </div>
            """))
            
            return result.result
            
        except Exception as e:
            execution_time = time.time() - start_time
            
            # Track the failed execution
            cell_id = f"cell_{int(time.time())}"
            integration.track_cell_execution(
                cell_id=cell_id,
                code=cell,
                output=f"ERROR: {str(e)}",
                execution_time=execution_time
            )
            
            display(HTML(f"""
            <div style="padding: 10px; background: #fab1a0; border-left: 4px solid #e17055; margin: 10px 0;">
                <strong>❌ Cell Failed</strong><br>
                Error: {str(e)}<br>
                Execution time: {execution_time:.2f}s<br>
                Cell ID: <code>{cell_id}</code>
            </div>
            """))
            
            raise
            
    @line_magic
    @magic_arguments()
    @argument('branches', nargs='*', help='Branch names to compare')
    def argon_compare(self, line):
        """
        Compare experiments across branches
        
        Usage: %argon_compare experiment-1 experiment-2 baseline
        """
        args = parse_argline(self.argon_compare, line)
        integration = get_argon_integration()
        
        if not integration:
            display(HTML("""
            <div style="padding: 10px; background: #fab1a0; border-left: 4px solid #e17055; margin: 10px 0;">
                <strong>❌ Error</strong><br>
                Argon not initialized. Use <code>%argon_init project-name</code> first.
            </div>
            """))
            return
            
        if len(args.branches) < 2:
            display(HTML("""
            <div style="padding: 10px; background: #ffeaa7; border-left: 4px solid #fdcb6e; margin: 10px 0;">
                <strong>⚠️ Error</strong><br>
                Please specify at least 2 branches to compare.
            </div>
            """))
            return
            
        comparison = integration.compare_experiments(args.branches)
        
        # Create comparison table
        comparison_html = "<table style='border-collapse: collapse; width: 100%;'>"
        comparison_html += "<tr style='background: #f1f1f1;'><th style='padding: 8px; border: 1px solid #ddd;'>Branch</th><th style='padding: 8px; border: 1px solid #ddd;'>Datasets</th><th style='padding: 8px; border: 1px solid #ddd;'>Models</th><th style='padding: 8px; border: 1px solid #ddd;'>Checkpoints</th></tr>"
        
        for branch in comparison["branches"]:
            comparison_html += f"""
            <tr>
                <td style='padding: 8px; border: 1px solid #ddd;'>{branch['name']}</td>
                <td style='padding: 8px; border: 1px solid #ddd;'>{branch['datasets']}</td>
                <td style='padding: 8px; border: 1px solid #ddd;'>{branch['models']}</td>
                <td style='padding: 8px; border: 1px solid #ddd;'>{branch['checkpoints']}</td>
            </tr>
            """
            
        comparison_html += "</table>"
        
        # Create metrics comparison
        metrics_html = ""
        if comparison["metrics_comparison"]:
            metrics_html = "<br><strong>Metrics Comparison:</strong><br>"
            for metric, values in comparison["metrics_comparison"].items():
                metrics_html += f"<br><strong>{metric}:</strong><br>"
                for value in values:
                    metrics_html += f"&nbsp;&nbsp;{value['branch']}: {value['value']}<br>"
        
        display(HTML(f"""
        <div style="padding: 15px; background: #f8f9fa; border-left: 4px solid #6c757d; margin: 10px 0;">
            <strong>📊 Branch Comparison</strong><br><br>
            {comparison_html}
            {metrics_html}
        </div>
        """))
        
        return comparison


def load_ipython_extension(ipython):
    """
    Load the Argon extension in IPython/Jupyter
    """
    ipython.register_magic_function(ArgonMagics.argon_init, 'line')
    ipython.register_magic_function(ArgonMagics.argon_branch, 'line')
    ipython.register_magic_function(ArgonMagics.argon_params, 'line')
    ipython.register_magic_function(ArgonMagics.argon_metrics, 'line')
    ipython.register_magic_function(ArgonMagics.argon_checkpoint, 'line')
    ipython.register_magic_function(ArgonMagics.argon_status, 'line')
    ipython.register_magic_function(ArgonMagics.argon_track, 'cell')
    ipython.register_magic_function(ArgonMagics.argon_compare, 'line')
    
    # Display welcome message
    from IPython.display import display, HTML
    display(HTML("""
    <div style="padding: 15px; background: #e8f5e8; border-left: 4px solid #4caf50; margin: 10px 0;">
        <strong>🚀 Argon Extension Loaded</strong><br>
        Available magic commands:<br>
        <ul>
            <li><code>%argon_init project-name</code> - Initialize Argon</li>
            <li><code>%argon_branch branch-name</code> - Set working branch</li>
            <li><code>%argon_params key=value ...</code> - Log parameters</li>
            <li><code>%argon_metrics key=value ...</code> - Log metrics</li>
            <li><code>%argon_checkpoint name</code> - Create checkpoint</li>
            <li><code>%argon_status</code> - Show status</li>
            <li><code>%%argon_track</code> - Track cell execution</li>
            <li><code>%argon_compare branch1 branch2 ...</code> - Compare experiments</li>
        </ul>
        Get started with <code>%argon_init your-project-name</code>
    </div>
    """))


def unload_ipython_extension(ipython):
    """
    Unload the Argon extension from IPython/Jupyter
    """
    # Clean up if needed
    pass