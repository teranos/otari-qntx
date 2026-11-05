# otaripy's handler: attested to the node's ATS as predicate handler, context
# otaripy, subject gateway, with this file as attributes.code. pyre reads @llm
# and @route off it at Initialize; the work is in the otaripy module the
# plugin's Python carries.

import otaripy


@llm()
def chat(request):
    return otaripy.chat(request, plugin_config)


@route('POST', '/v1/chat/completions')
def completions(request):
    return otaripy.completions(request, plugin_config)


@route('GET', '/usage')
def usage(request):
    return otaripy.usage(request, plugin_config)
