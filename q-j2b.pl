local $/; my $f='repartition.c'; open H,'<',$f or die; my $s=<H>; close H;
sub rep { my ($o,$n)=@_; my $i=index($s,$o); die "nf: $o" if $i<0; substr($s,$i,length $o)=$n; }
# helper before rp_collect comment
rep("/* The conjuncts of an inner-join tree; an outer join is refused (J2). */",
"/*\n * Does the condition name the columns of the table r and no other table?  (pull_varnos also returns the join entries that a\n * column of the nullable side of an outer join is nulled by, so it is not used here.)\n */\nstatic bool\nrp_names_only(Node *qual, Index r)\n{\n	List	   *vars = pull_var_clause(qual, PVC_RECURSE_AGGREGATES | PVC_RECURSE_WINDOWFUNCS | PVC_RECURSE_PLACEHOLDERS);\n	ListCell   *lc;\n\n	if (vars == NIL)\n		return false;\n	foreach(lc, vars)\n	{\n		Var		   *v = (Var *) lfirst(lc);\n\n		if (!IsA(v, Var) || v->varlevelsup != 0 || v->varno != r)\n			return false;\n	}\n	return true;\n}\n\n/* The conjuncts of the join tree: inner joins, and one LEFT JOIN to a table (J2). */");
rep("			Bitmapset  *vs = pull_varnos(NULL, (Node *) lfirst(lc));\n\n			if (!list_member_ptr(c.outer_on, lfirst(lc)) && bms_membership(vs) == BMS_SINGLETON &&\n				bms_is_member((int) c.outer_rti, vs))\n",
"			if (!list_member_ptr(c.outer_on, lfirst(lc)) && rp_names_only((Node *) lfirst(lc), c.outer_rti))\n");
rep("		Bitmapset  *vs = pull_varnos(NULL, (Node *) lfirst(lc));\n\n		if (bms_membership(vs) == BMS_SINGLETON && bms_is_member((int) r, vs))\n			continue;",
"		if (rp_names_only((Node *) lfirst(lc), r))\n			continue;");
open H,'>',$f; print H $s; close H;
