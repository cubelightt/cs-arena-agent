#! /bin/bash
## vim: noet:sw=0:sts=0:ts=4

SourcemodPackage.counterstrikesharp::download () {
	SourcemodHelper::unpackZip \
		https://github.com/roflmuffin/CounterStrikeSharp/releases/download/v1.0.374/counterstrikesharp-with-runtime-linux-1.0.374.zip \
		22a24aa482e226ad0dcdc72cec03e1927d267eb5a1566094e98809a06dcc6792
}
